// Runs the real KMS snapshot code against a deterministic ioctl driver model.
#define ioctl fixture_ioctl
#define close fixture_close
#include "../kms_linux.c"
#undef ioctl
#undef close
#include <assert.h>
#include <stdarg.h>

static int handles_closed, fd_closed, scenario;
int fixture_close(int fd) {
    assert(fd == 93);
    fd_closed++;
    return 0;
}
int fixture_ioctl(int fd, unsigned long request, ...) {
    (void)fd;
    va_list ap;
    va_start(ap, request);
    void *arg = va_arg(ap, void *);
    va_end(ap);
    if (request == DRM_IOCTL_MODE_GETCRTC) {
        struct drm_mode_crtc *p = arg;
        p->mode_valid = 1;
        p->mode.hdisplay = 1280;
        p->mode.vdisplay = 720;
        p->mode.clock = 74250;
        p->mode.htotal = 1650;
        p->mode.vtotal = 750;
        if (scenario == 4)
            p->mode.hdisplay = 1920;
        return 0;
    }
    if (request == DRM_IOCTL_MODE_GETPLANE) {
        struct drm_mode_get_plane *p = arg;
        p->crtc_id = 2;
        p->fb_id = 3;
        return 0;
    }
    if (request == DRM_IOCTL_MODE_GETFB2) {
        struct drm_mode_fb_cmd2 *p = arg;
        p->handles[0] = 77;
        p->width = 1280;
        p->height = 720;
        p->pitches[0] = 5120;
        p->pixel_format = DRM_FORMAT_XRGB8888;
        if (scenario == 1)
            p->pixel_format = DRM_FORMAT_NV12;
        if (scenario == 2)
            p->handles[0] = 0;
        return 0;
    }
    if (request == DRM_IOCTL_PRIME_HANDLE_TO_FD) {
        struct drm_prime_handle *p = arg;
        assert(p->handle == 77);
        p->fd = 93;
        return 0;
    }
    if (request == DRM_IOCTL_GEM_CLOSE) {
        struct drm_gem_close *p = arg;
        assert(p->handle == 77);
        handles_closed++;
        return 0;
    }
    if (request == DRM_IOCTL_MODE_OBJ_GETPROPERTIES) {
        struct drm_mode_obj_get_properties *p = arg;
        if (p->props_ptr) {
            uint32_t *ids = (void *)(uintptr_t)p->props_ptr;
            uint64_t *values = (void *)(uintptr_t)p->prop_values_ptr;
            for (int i = 0; i < 5; i++)
                ids[i] = i + 1;
            values[0] = values[1] = 0;
            values[2] = 1280ull << 16;
            values[3] = 720ull << 16;
            values[4] = DRM_MODE_ROTATE_90;
            if (scenario == 3)
                values[0] = 1;
        }
        p->count_props = 5;
        return 0;
    }
    if (request == DRM_IOCTL_MODE_GETPROPERTY) {
        static const char *names[] = {"SRC_X", "SRC_Y", "SRC_W", "SRC_H", "rotation"};
        struct drm_mode_get_property *p = arg;
        strcpy(p->name, names[p->prop_id - 1]);
        return 0;
    }
    assert(!"unexpected ioctl");
    return -1;
}
int main(void) {
    bk_kms k = {
        .fd = 10,
        .output = {.crtc = 2, .plane = 1, .width = 1280, .height = 720, .rate_num = 74250000, .rate_den = 1650 * 750}};
    for (scenario = 0; scenario < 5; scenario++) {
        handles_closed = fd_closed = 0;
        char error[512] = {0};
        bk_kms_frame f;
        int result = bk_kms_next(&k, &f, error, sizeof(error));
        if (scenario == 0) {
            assert(result == 0 && f.fd == 93 && f.rotation == 270 && f.crop_width == 1280 && handles_closed == 1 &&
                   !fd_closed);
            fixture_close(f.fd);
        } else {
            assert(result < 0 && error[0] && f.fd == -1);
            assert(handles_closed == (scenario == 2 || scenario == 4 ? 0 : 1));
            assert(fd_closed == (scenario == 3 ? 1 : 0));
        }
    }
    return 0;
}
