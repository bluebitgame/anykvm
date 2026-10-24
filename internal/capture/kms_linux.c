//go:build linux && cgo

#include "kms_linux.h"
// Sparse kernel pointer annotations have no userspace ABI effect.
#ifndef __user
#define __user
#endif
#include "third_party/drm/drm.h"
#include "third_party/drm/drm_fourcc.h"
#include "third_party/drm/drm_mode.h"
#include <errno.h>
#include <fcntl.h>
#include <glob.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/stat.h>
#ifdef __linux__
#include <sys/sysmacros.h>
#endif
#include <unistd.h>
#include <time.h>

struct bk_kms {
    int fd;
    bk_kms_output output;
};
static int call(int fd, unsigned long request, void *arg) {
    int result;
    do {
        result = ioctl(fd, request, arg);
    } while (result < 0 && errno == EINTR);
    return result;
}
static int failure(char *error, size_t capacity, const char *operation) {
    snprintf(error, capacity, "KMS %s: %s%s", operation, strerror(errno),
             errno == EACCES || errno == EPERM
                 ? " (DRM framebuffer access requires appropriate device permissions and CAP_SYS_ADMIN)"
                 : "");
    return -1;
}
static int properties(int fd, uint32_t object, uint32_t type, uint32_t **ids, uint64_t **values, uint32_t *count) {
    struct drm_mode_obj_get_properties p = {.obj_id = object, .obj_type = type};
    if (call(fd, DRM_IOCTL_MODE_OBJ_GETPROPERTIES, &p) < 0 || p.count_props > 256)
        return -1;
    uint32_t limit = p.count_props;
    *ids = calloc(limit ? limit : 1, sizeof(**ids));
    *values = calloc(limit ? limit : 1, sizeof(**values));
    if (!*ids || !*values)
        goto failed;
    p.props_ptr = (uintptr_t)*ids;
    p.prop_values_ptr = (uintptr_t)*values;
    if (call(fd, DRM_IOCTL_MODE_OBJ_GETPROPERTIES, &p) < 0 || p.count_props > limit)
        goto failed;
    *count = p.count_props;
    return 0;
failed:
    free(*ids);
    free(*values);
    *ids = NULL;
    *values = NULL;
    return -1;
}
static int property(int fd, uint32_t object, uint32_t type, const char *name, uint64_t *value) {
    uint32_t *ids = NULL, count = 0;
    uint64_t *values = NULL;
    int found = 0;
    if (properties(fd, object, type, &ids, &values, &count) < 0)
        return -1;
    for (uint32_t i = 0; i < count; i++) {
        struct drm_mode_get_property p = {.prop_id = ids[i]};
        if (call(fd, DRM_IOCTL_MODE_GETPROPERTY, &p) == 0 && !strncmp(p.name, name, sizeof(p.name))) {
            *value = values[i];
            found = 1;
            break;
        }
    }
    free(ids);
    free(values);
    return found;
}
static uint32_t primary_plane(int fd, uint32_t crtc) {
    struct drm_mode_get_plane_res res = {0};
    if (call(fd, DRM_IOCTL_MODE_GETPLANERESOURCES, &res) < 0 || !res.count_planes || res.count_planes > 256)
        return 0;
    uint32_t count = res.count_planes, *planes = calloc(count, sizeof(*planes)), result = 0;
    if (!planes)
        return 0;
    res.plane_id_ptr = (uintptr_t)planes;
    if (call(fd, DRM_IOCTL_MODE_GETPLANERESOURCES, &res) < 0 || res.count_planes > count)
        goto done;
    for (uint32_t i = 0; i < res.count_planes; i++) {
        struct drm_mode_get_plane p = {.plane_id = planes[i]};
        uint64_t type = 0;
        if (call(fd, DRM_IOCTL_MODE_GETPLANE, &p) == 0 && p.crtc_id == crtc && p.fb_id &&
            property(fd, p.plane_id, DRM_MODE_OBJECT_PLANE, "type", &type) == 1 && type == 1) {
            result = p.plane_id;
            break;
        }
    }
done:
    free(planes);
    return result;
}
// Two-pass UAPI enumeration is bounded and rejects a hotplug which changes the
// allocation size. No modeset, DRM-master acquisition, or display mutation.
static int enumerate_card(int fd, const char *path, bk_kms_output *outputs, int capacity, int *total) {
    struct drm_mode_card_res res = {0};
    if (call(fd, DRM_IOCTL_MODE_GETRESOURCES, &res) < 0 || res.count_connectors > 256)
        return -1;
    uint32_t count = res.count_connectors, *ids = calloc(count ? count : 1, sizeof(*ids));
    if (!ids)
        return -1;
    res.connector_id_ptr = (uintptr_t)ids;
    // No arrays for unused resource lists are requested.
    res.count_fbs = res.count_crtcs = res.count_encoders = 0;
    if (call(fd, DRM_IOCTL_MODE_GETRESOURCES, &res) < 0 || res.count_connectors > count) {
        free(ids);
        return -1;
    }
    struct stat stat;
    fstat(fd, &stat);
    for (uint32_t i = 0; i < res.count_connectors; i++) {
        struct drm_mode_get_connector conn = {.connector_id = ids[i]};
        if (call(fd, DRM_IOCTL_MODE_GETCONNECTOR, &conn) < 0 || conn.connection != 1 || !conn.encoder_id)
            continue;
        struct drm_mode_get_encoder encoder = {.encoder_id = conn.encoder_id};
        if (call(fd, DRM_IOCTL_MODE_GETENCODER, &encoder) < 0 || !encoder.crtc_id)
            continue;
        struct drm_mode_crtc crtc = {.crtc_id = encoder.crtc_id};
        if (call(fd, DRM_IOCTL_MODE_GETCRTC, &crtc) < 0 || !crtc.mode_valid)
            continue;
        uint32_t plane = primary_plane(fd, crtc.crtc_id);
        if (!plane)
            continue;
        if (*total < capacity) {
            bk_kms_output *o = &outputs[*total];
            memset(o, 0, sizeof(*o));
            snprintf(o->card, sizeof(o->card), "%s", path);
            o->connector = conn.connector_id;
            o->crtc = crtc.crtc_id;
            o->plane = plane;
            o->width = crtc.mode.hdisplay;
            o->height = crtc.mode.vdisplay;
            o->rate_num = crtc.mode.clock * 1000u;
            o->rate_den = (uint32_t)crtc.mode.htotal * crtc.mode.vtotal;
            if (crtc.mode.flags & DRM_MODE_FLAG_INTERLACE)
                o->rate_num *= 2;
            if (crtc.mode.flags & DRM_MODE_FLAG_DBLSCAN)
                o->rate_den *= 2;
            if (crtc.mode.vscan > 1)
                o->rate_den *= crtc.mode.vscan;
            o->device_major = major(stat.st_rdev);
            o->device_minor = minor(stat.st_rdev);
        }
        (*total)++;
    }
    free(ids);
    return 0;
}
static int prepare(int fd) {
    struct drm_set_client_cap cap = {.capability = DRM_CLIENT_CAP_UNIVERSAL_PLANES, .value = 1};
    return call(fd, DRM_IOCTL_SET_CLIENT_CAP, &cap);
}
int bk_kms_list(bk_kms_output *outputs, int capacity, char *error, size_t size) {
    glob_t cards = {0};
    int total = 0, denied = 0;
    if (glob("/dev/dri/card[0-9]*", 0, NULL, &cards) != 0) {
        snprintf(error, size, "KMS: no DRM card devices found");
        return -1;
    }
    for (size_t i = 0; i < cards.gl_pathc; i++) {
        int fd = open(cards.gl_pathv[i], O_RDWR | O_CLOEXEC);
        if (fd < 0) {
            if (errno == EACCES || errno == EPERM)
                denied = 1;
            continue;
        }
        if (prepare(fd) == 0)
            enumerate_card(fd, cards.gl_pathv[i], outputs, capacity, &total);
        close(fd);
    }
    globfree(&cards);
    if (!total) {
        snprintf(error, size, "KMS: no active primary display planes%s", denied ? "; DRM device access denied" : "");
        return -1;
    }
    return total;
}
bk_kms *bk_kms_open(int index, bk_kms_output *output, char *error, size_t capacity) {
    bk_kms_output outputs[64];
    int count = bk_kms_list(outputs, 64, error, capacity);
    if (count < 0)
        return NULL;
    if (index < 0 || index >= count || index >= 64) {
        snprintf(error, capacity, "KMS: screen index %d is unavailable", index);
        return NULL;
    }
    bk_kms *k = calloc(1, sizeof(*k));
    if (!k)
        return NULL;
    k->output = outputs[index];
    k->fd = -1;
    k->fd = open(k->output.card, O_RDWR | O_CLOEXEC);
    if (k->fd < 0 || prepare(k->fd) < 0) {
        failure(error, capacity, "open card");
        bk_kms_close(k);
        return NULL;
    }
    *output = k->output;
    return k;
}
int bk_kms_next(bk_kms *k, bk_kms_frame *out, char *error, size_t capacity) {
    memset(out, 0, sizeof(*out));
    out->fd = -1;
    struct timespec acquired;
    if (clock_gettime(CLOCK_MONOTONIC, &acquired) < 0)
        return failure(error, capacity, "read capture clock");
    out->pts_us = (int64_t)acquired.tv_sec * 1000000 + acquired.tv_nsec / 1000;
    struct drm_mode_crtc crtc = {.crtc_id = k->output.crtc};
    struct drm_mode_get_plane plane = {.plane_id = k->output.plane};
    if (call(k->fd, DRM_IOCTL_MODE_GETCRTC, &crtc) < 0 || !crtc.mode_valid || crtc.mode.hdisplay != k->output.width ||
        crtc.mode.vdisplay != k->output.height ||
        (uint64_t)crtc.mode.clock * 1000u * ((crtc.mode.flags & DRM_MODE_FLAG_INTERLACE) ? 2 : 1) *
                k->output.rate_den !=
            (uint64_t)k->output.rate_num * crtc.mode.htotal * crtc.mode.vtotal *
                ((crtc.mode.flags & DRM_MODE_FLAG_DBLSCAN) ? 2 : 1) * (crtc.mode.vscan > 1 ? crtc.mode.vscan : 1) ||
        call(k->fd, DRM_IOCTL_MODE_GETPLANE, &plane) < 0 || plane.crtc_id != k->output.crtc || !plane.fb_id) {
        snprintf(error, capacity, "KMS: display changed or disconnected; reopen capture");
        return -1;
    }
    struct drm_mode_fb_cmd2 fb = {.fb_id = plane.fb_id};
    if (call(k->fd, DRM_IOCTL_MODE_GETFB2, &fb) < 0)
        return failure(error, capacity, "get framebuffer");
    int result = -1;
    if (!fb.handles[0]) {
        errno = EACCES;
        failure(error, capacity, "export framebuffer");
        goto done;
    }
    if (fb.pixel_format != DRM_FORMAT_XRGB8888 && fb.pixel_format != DRM_FORMAT_ARGB8888 &&
        fb.pixel_format != DRM_FORMAT_XBGR8888 && fb.pixel_format != DRM_FORMAT_ABGR8888) {
        snprintf(error, capacity, "KMS: unsupported primary framebuffer format 0x%08x (requires SDR RGB8888)",
                 fb.pixel_format);
        goto done;
    }
    if (!fb.width || !fb.height || fb.width > 16384 || fb.height > 16384) {
        snprintf(error, capacity, "KMS: unsupported primary framebuffer layout");
        goto done;
    }
    for (int i = 0; i < 4; i++)
        if (fb.handles[i]) {
            if (fb.handles[i] != fb.handles[0]) {
                snprintf(error, capacity, "KMS: modifier planes use multiple memory objects; unsupported");
                goto done;
            }
            out->planes = i + 1;
            out->pitches[i] = fb.pitches[i];
            out->offsets[i] = fb.offsets[i];
        }
    struct drm_prime_handle prime = {.handle = fb.handles[0], .flags = DRM_CLOEXEC | DRM_RDWR, .fd = -1};
    if (call(k->fd, DRM_IOCTL_PRIME_HANDLE_TO_FD, &prime) < 0) {
        failure(error, capacity, "export DMA-BUF");
        goto done;
    }
    out->fd = prime.fd;
    out->width = fb.width;
    out->height = fb.height;
    out->fourcc = fb.pixel_format;
    out->pitch = fb.pitches[0];
    out->offset = fb.offsets[0];
    out->fb_id = fb.fb_id;
    out->modifier = (fb.flags & DRM_MODE_FB_MODIFIERS) ? fb.modifier[0] : 0;
    off_t end = lseek(out->fd, 0, SEEK_END);
    if (end > 0)
        out->size = (uint64_t)end;
    const char *names[] = {"SRC_X", "SRC_Y", "SRC_W", "SRC_H", "rotation"};
    uint64_t values[] = {0, 0, (uint64_t)fb.width << 16, (uint64_t)fb.height << 16, DRM_MODE_ROTATE_0};
    for (int i = 0; i < 5; i++)
        property(k->fd, plane.plane_id, DRM_MODE_OBJECT_PLANE, names[i], &values[i]);
    if ((values[0] | values[1] | values[2] | values[3]) & 65535 ||
        values[4] & (DRM_MODE_REFLECT_X | DRM_MODE_REFLECT_Y)) {
        snprintf(error, capacity, "KMS: fractional crop or reflected primary plane is unsupported");
        goto done;
    }
    out->x = values[0] >> 16;
    out->y = values[1] >> 16;
    out->crop_width = values[2] >> 16;
    out->crop_height = values[3] >> 16;
    out->rotation = values[4] & DRM_MODE_ROTATE_90    ? 270
                    : values[4] & DRM_MODE_ROTATE_180 ? 180
                    : values[4] & DRM_MODE_ROTATE_270 ? 90
                                                      : 0;
    if (!out->crop_width || !out->crop_height || out->x + out->crop_width > out->width ||
        out->y + out->crop_height > out->height) {
        snprintf(error, capacity, "KMS: primary plane crop is outside the framebuffer");
        goto done;
    }
    result = 0;
done:
    // GETFB2 creates GEM handle references. Close each unique handle even when
    // export/format validation fails. The exported DMA-BUF owns its own lease.
    for (int i = 0; i < 4; i++)
        if (fb.handles[i]) {
            int duplicate = 0;
            for (int j = 0; j < i; j++)
                if (fb.handles[j] == fb.handles[i])
                    duplicate = 1;
            if (!duplicate) {
                struct drm_gem_close c = {.handle = fb.handles[i]};
                call(k->fd, DRM_IOCTL_GEM_CLOSE, &c);
            }
        }
    if (result && out->fd >= 0) {
        close(out->fd);
        out->fd = -1;
    }
    return result;
}
void bk_kms_close(bk_kms *k) {
    if (k) {
        if (k->fd >= 0)
            close(k->fd);
        free(k);
    }
}
