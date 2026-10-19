//go:build linux && cgo

#include "native_v4l2_linux.h"
#include <time.h>
#include <linux/videodev2.h>
#include <libv4lconvert.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <fcntl.h>
#include <unistd.h>
#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <math.h>
#include <limits.h>
#define FRAME_LIMIT (128u * 1024u * 1024u)
static int call(int fd, unsigned long op, void *arg) {
    int r;
    do {
        r = ioctl(fd, op, arg);
    } while (r < 0 && errno == EINTR);
    return r;
}
static void fail(char *out, size_t n, const char *step) {
    snprintf(out, n, "%s: %s", step, strerror(errno));
}
static uint32_t caps(struct v4l2_capability *c) {
    return c->capabilities & V4L2_CAP_DEVICE_CAPS ? c->device_caps : c->capabilities;
}
int bk_v4l_candidate(const char *path) {
    int fd = open(path, O_RDONLY | O_NONBLOCK | O_CLOEXEC);
    if (fd < 0)
        return 0;
    struct v4l2_capability c = {0};
    int yes = call(fd, VIDIOC_QUERYCAP, &c) == 0 && (caps(&c) & V4L2_CAP_VIDEO_CAPTURE) &&
              (caps(&c) & V4L2_CAP_STREAMING) &&
              !(caps(&c) & (V4L2_CAP_VIDEO_M2M | V4L2_CAP_VIDEO_M2M_MPLANE));
    close(fd);
    return yes;
}
static void format_name(uint32_t code, char *out, size_t n) {
    const char *name = NULL;
    switch (code) {
    case V4L2_PIX_FMT_MJPEG:
        name = "mjpeg";
        break;
    case V4L2_PIX_FMT_JPEG:
        name = "jpeg";
        break;
    case V4L2_PIX_FMT_YUYV:
        name = "yuyv422";
        break;
    case V4L2_PIX_FMT_UYVY:
        name = "uyvy422";
        break;
    case V4L2_PIX_FMT_NV12:
        name = "nv12";
        break;
    case V4L2_PIX_FMT_YUV420:
        name = "i420";
        break;
    case V4L2_PIX_FMT_YVU420:
        name = "yv12";
        break;
    case V4L2_PIX_FMT_BGR24:
        name = "bgr24";
        break;
    case V4L2_PIX_FMT_BGR32:
        name = "bgr0";
        break;
    case V4L2_PIX_FMT_ABGR32:
        name = "bgra";
        break;
    case V4L2_PIX_FMT_H264:
        name = "h264";
        break;
    default:
        break;
    }
    if (name)
        snprintf(out, n, "%s", name);
    else
        snprintf(out, n, "fourcc_%08x", code);
}
static double fps(struct v4l2_fract f) {
    return f.numerator ? (double)f.denominator / f.numerator : 0;
}
static int append_mode(char **json, size_t *used, uint32_t w, uint32_t h, uint32_t code, double min,
                       double max) {
    if (!w || !h || w > 65535 || h > 65535 || min <= 0 || max < min || max > 1000)
        return 0;
    char name[40], row[256];
    format_name(code, name, sizeof(name));
    int n = snprintf(
        row, sizeof(row),
        "%s{\"width\":%u,\"height\":%u,\"pixelFormat\":\"%s\",\"minFPS\":%.12g,\"maxFPS\":%.12g}",
        *used > 1 ? "," : "", w, h, name, min, max);
    if (n < 0 || (size_t)n >= sizeof(row) || *used + (size_t)n + 2 > FRAME_LIMIT) {
        errno = ENOMEM;
        return -1;
    }
    char *next = realloc(*json, *used + (size_t)n + 2);
    if (!next)
        return -1;
    *json = next;
    memcpy(next + *used, row, (size_t)n);
    *used += (size_t)n;
    next[*used] = 0;
    return 0;
}
static int intervals(int fd, uint32_t code, uint32_t w, uint32_t h, char **json, size_t *used) {
    struct v4l2_frmivalenum iv = {0};
    iv.pixel_format = code;
    iv.width = w;
    iv.height = h;
    for (iv.index = 0;; iv.index++) {
        if (call(fd, VIDIOC_ENUM_FRAMEINTERVALS, &iv) < 0) {
            if (errno != EINVAL && errno != ENOTTY)
                return -1;
            if (iv.index == 0) {
                struct v4l2_format current = {0};
                current.type = V4L2_BUF_TYPE_VIDEO_CAPTURE;
                struct v4l2_streamparm p = {0};
                p.type = V4L2_BUF_TYPE_VIDEO_CAPTURE;
                if (call(fd, VIDIOC_G_FMT, &current) == 0 && current.fmt.pix.width == w &&
                    current.fmt.pix.height == h && current.fmt.pix.pixelformat == code &&
                    call(fd, VIDIOC_G_PARM, &p) == 0) {
                    double r = fps(p.parm.capture.timeperframe);
                    return append_mode(json, used, w, h, code, r, r);
                }
            }
            return 0;
        }
        if (iv.type == V4L2_FRMIVAL_TYPE_DISCRETE) {
            double r = fps(iv.discrete);
            if (append_mode(json, used, w, h, code, r, r) < 0)
                return -1;
        } else if (iv.type == V4L2_FRMIVAL_TYPE_CONTINUOUS) {
            return append_mode(json, used, w, h, code, fps(iv.stepwise.max), fps(iv.stepwise.min));
        } else if (iv.type == V4L2_FRMIVAL_TYPE_STEPWISE) {
            struct v4l2_frmival_stepwise r = iv.stepwise;
            if (!r.min.denominator || !r.max.denominator || !r.step.denominator ||
                !r.step.numerator)
                return 0;
            long double min = (long double)r.min.numerator / r.min.denominator,
                        max = (long double)r.max.numerator / r.max.denominator,
                        step = (long double)r.step.numerator / r.step.denominator;
            if (min <= 0 || max < min)
                return 0;
            long double count = floorl((max - min) / step + 0.0000001L) + 1;
            if (count <= 1024) {
                for (unsigned j = 0; j < (unsigned)count; j++) {
                    double rate = (double)(1 / (min + j * step));
                    if (append_mode(json, used, w, h, code, rate, rate) < 0)
                        return -1;
                }
            } else {
                double first = fps(r.min), last = fps(r.max);
                if (append_mode(json, used, w, h, code, first, first) < 0 ||
                    append_mode(json, used, w, h, code, last, last) < 0)
                    return -1;
                const double common[] = {15, 20, 24, 25, 30, 48, 50, 60, 90, 120, 144, 240};
                for (unsigned j = 0; j < sizeof(common) / sizeof(common[0]); j++) {
                    long double period = 1.0L / common[j], index = roundl((period - min) / step);
                    if (period >= min && period <= max &&
                        fabsl(min + index * step - period) < 0.000000001L)
                        if (append_mode(json, used, w, h, code, common[j], common[j]) < 0)
                            return -1;
                }
            }
            return 0;
        } else
            return 0;
    }
}
char *bk_v4l_modes(const char *path, char *error, size_t n) {
    int fd = open(path, O_RDONLY | O_NONBLOCK | O_CLOEXEC);
    char *json = NULL;
    size_t used = 1;
    if (fd < 0) {
        fail(error, n, "open V4L2 metadata");
        return NULL;
    }
    struct v4l2_capability cap = {0};
    if (call(fd, VIDIOC_QUERYCAP, &cap) < 0)
        goto failed;
    if (!(caps(&cap) & V4L2_CAP_VIDEO_CAPTURE) ||
        (caps(&cap) & (V4L2_CAP_VIDEO_M2M | V4L2_CAP_VIDEO_M2M_MPLANE))) {
        errno = ENOTSUP;
        goto failed;
    }
    json = malloc(3);
    if (!json)
        goto failed;
    json[0] = '[';
    json[1] = 0;
    struct v4l2_fmtdesc fmt = {0};
    fmt.type = V4L2_BUF_TYPE_VIDEO_CAPTURE;
    for (fmt.index = 0;; fmt.index++) {
        if (call(fd, VIDIOC_ENUM_FMT, &fmt) < 0) {
            if (errno == EINVAL)
                break;
            goto failed;
        }
        struct v4l2_frmsizeenum sz = {0};
        sz.pixel_format = fmt.pixelformat;
        for (sz.index = 0;; sz.index++) {
            if (call(fd, VIDIOC_ENUM_FRAMESIZES, &sz) < 0) {
                if (errno != EINVAL && errno != ENOTTY)
                    goto failed;
                if (sz.index == 0) {
                    struct v4l2_format cur = {0};
                    cur.type = fmt.type;
                    if (call(fd, VIDIOC_G_FMT, &cur) == 0 &&
                        cur.fmt.pix.pixelformat == fmt.pixelformat &&
                        intervals(fd, fmt.pixelformat, cur.fmt.pix.width, cur.fmt.pix.height, &json,
                                  &used) < 0)
                        goto failed;
                }
                break;
            }
            if (sz.type == V4L2_FRMSIZE_TYPE_DISCRETE) {
                if (intervals(fd, fmt.pixelformat, sz.discrete.width, sz.discrete.height, &json,
                              &used) < 0)
                    goto failed;
            } else if (sz.type == V4L2_FRMSIZE_TYPE_STEPWISE ||
                       sz.type == V4L2_FRMSIZE_TYPE_CONTINUOUS) {
                // A continuous size space is represented by legal common sizes and endpoints.
                // Custom sizes still pass through exact S_FMT validation when selected.
                const unsigned common[][2] = {{320, 240},   {640, 480},   {800, 600},
                                              {1024, 768},  {1280, 720},  {1280, 1024},
                                              {1920, 1080}, {1920, 1200}, {2560, 1440},
                                              {3840, 2160}, {4096, 2160}, {7680, 4320}};
                struct v4l2_frmsize_stepwise r = sz.stepwise;
                if (intervals(fd, fmt.pixelformat, r.min_width, r.min_height, &json, &used) < 0 ||
                    intervals(fd, fmt.pixelformat, r.max_width, r.max_height, &json, &used) < 0)
                    goto failed;
                for (unsigned j = 0; j < sizeof(common) / sizeof(common[0]); j++) {
                    unsigned w = common[j][0], h = common[j][1];
                    if (w >= r.min_width && w <= r.max_width && h >= r.min_height &&
                        h <= r.max_height &&
                        (!r.step_width || (w - r.min_width) % r.step_width == 0) &&
                        (!r.step_height || (h - r.min_height) % r.step_height == 0))
                        if (intervals(fd, fmt.pixelformat, w, h, &json, &used) < 0)
                            goto failed;
                }
                break;
            }
        }
    }
    json[used++] = ']';
    json[used] = 0;
    close(fd);
    return json;
failed:
    fail(error, n, "V4L2 mode enumeration");
    free(json);
    close(fd);
    return NULL;
}
struct mapped_buffer {
    void *data;
    size_t size;
};
struct bk_v4l_capture {
    int fd, streaming;
    unsigned count;
    struct mapped_buffer *buffers;
    struct v4l2_format source, destination;
    struct v4l2_fract interval;
    struct v4lconvert_data *converter;
    uint8_t *rgb;
    size_t rgb_size;
    char format[40];
};
void bk_v4l_close(bk_v4l_capture *c) {
    if (!c)
        return;
    if (c->streaming) {
        enum v4l2_buf_type t = V4L2_BUF_TYPE_VIDEO_CAPTURE;
        call(c->fd, VIDIOC_STREAMOFF, &t);
    }
    if (c->buffers)
        for (unsigned i = 0; i < c->count; i++)
            if (c->buffers[i].data && c->buffers[i].data != MAP_FAILED)
                munmap(c->buffers[i].data, c->buffers[i].size);
    if (c->converter)
        v4lconvert_destroy(c->converter);
    free(c->buffers);
    free(c->rgb);
    if (c->fd >= 0)
        close(c->fd);
    free(c);
}
// Fixed-rate drivers may expose intervals while omitting G_PARM/S_PARM.
static int fixed_interval(int fd, struct v4l2_pix_format *p, struct v4l2_fract *period) {
    struct v4l2_frmivalenum iv = {0};
    iv.pixel_format = p->pixelformat;
    iv.width = p->width;
    iv.height = p->height;
    if (call(fd, VIDIOC_ENUM_FRAMEINTERVALS, &iv) < 0 || iv.type != V4L2_FRMIVAL_TYPE_DISCRETE ||
        fps(iv.discrete) <= 0)
        return 0;
    struct v4l2_fract only = iv.discrete;
    iv.index = 1;
    if (call(fd, VIDIOC_ENUM_FRAMEINTERVALS, &iv) < 0 && errno == EINVAL) {
        *period = only;
        return 1;
    }
    return 0;
}
static struct v4l2_fract requested_interval(int fd, struct v4l2_pix_format *p, double rate) {
    struct v4l2_frmivalenum iv = {0};
    iv.pixel_format = p->pixelformat;
    iv.width = p->width;
    iv.height = p->height;
    for (iv.index = 0; call(fd, VIDIOC_ENUM_FRAMEINTERVALS, &iv) == 0; iv.index++) {
        if (iv.type != V4L2_FRMIVAL_TYPE_DISCRETE)
            break;
        if (fabs(fps(iv.discrete) - rate) <= 0.001)
            return iv.discrete;
    }
    unsigned a = 1000000, b = (unsigned)llround(rate * 1000000), x = a, y = b;
    while (y) {
        unsigned t = x % y;
        x = y;
        y = t;
    }
    return (struct v4l2_fract){a / x, b / x};
}
bk_v4l_capture *bk_v4l_open(const char *path, int w, int h, double rate, const char *format,
                            char *error, size_t n) {
    const char *step = "allocate V4L2 capture";
    bk_v4l_capture *c = calloc(1, sizeof(*c));
    if (!c) {
        fail(error, n, step);
        return NULL;
    }
    c->fd = -1;
    step = "open V4L2 input";
    c->fd = open(path, O_RDWR | O_NONBLOCK | O_CLOEXEC);
    if (c->fd < 0)
        goto failed;
    struct v4l2_capability cap = {0};
    step = "query V4L2 capture capabilities";
    if (call(c->fd, VIDIOC_QUERYCAP, &cap) < 0)
        goto failed;
    if (!(caps(&cap) & V4L2_CAP_VIDEO_CAPTURE) || !(caps(&cap) & V4L2_CAP_STREAMING) ||
        (caps(&cap) & (V4L2_CAP_VIDEO_M2M | V4L2_CAP_VIDEO_M2M_MPLANE))) {
        errno = ENOTSUP;
        goto failed;
    }
    c->source.type = V4L2_BUF_TYPE_VIDEO_CAPTURE;
    step = "read current V4L2 input format";
    if (call(c->fd, VIDIOC_G_FMT, &c->source) < 0)
        goto failed;
    struct v4l2_pix_format original = c->source.fmt.pix;
    if (w > 0)
        c->source.fmt.pix.width = (unsigned)w;
    if (h > 0)
        c->source.fmt.pix.height = (unsigned)h;
    if (*format) {
        int found = 0;
        struct v4l2_fmtdesc f = {0};
        f.type = c->source.type;
        for (f.index = 0; call(c->fd, VIDIOC_ENUM_FMT, &f) == 0; f.index++) {
            char name[40];
            format_name(f.pixelformat, name, sizeof(name));
            if (!strcmp(name, format)) {
                c->source.fmt.pix.pixelformat = f.pixelformat;
                found = 1;
                break;
            }
        }
        if (!found) {
            step = "unsupported V4L2 input pixel format";
            errno = EINVAL;
            goto failed;
        }
    }
    struct v4l2_pix_format wanted = c->source.fmt.pix;
    step = "set exact V4L2 resolution and pixel format";
    if ((wanted.width != original.width || wanted.height != original.height ||
         wanted.pixelformat != original.pixelformat) &&
        call(c->fd, VIDIOC_S_FMT, &c->source) < 0)
        goto failed;
    if (c->source.fmt.pix.width != wanted.width || c->source.fmt.pix.height != wanted.height ||
        c->source.fmt.pix.pixelformat != wanted.pixelformat) {
        errno = ENOTSUP;
        goto failed;
    }
    format_name(c->source.fmt.pix.pixelformat, c->format, sizeof(c->format));
    struct v4l2_streamparm p = {0};
    p.type = c->source.type;
    step = "read V4L2 frame rate";
    int has_rate = call(c->fd, VIDIOC_G_PARM, &p) == 0;
    if (!has_rate || fps(p.parm.capture.timeperframe) <= 0)
        has_rate = fixed_interval(c->fd, &c->source.fmt.pix, &p.parm.capture.timeperframe);
    if (rate > 0 && (!has_rate || fabs(fps(p.parm.capture.timeperframe) - rate) > 0.001)) {
        step = "set exact V4L2 frame rate";
        if (!has_rate)
            memset(&p, 0, sizeof(p));
        p.type = c->source.type;
        p.parm.capture.timeperframe = requested_interval(c->fd, &c->source.fmt.pix, rate);
        if (call(c->fd, VIDIOC_S_PARM, &p) < 0)
            goto failed;
        if (fabs(fps(p.parm.capture.timeperframe) - rate) > 0.001) {
            errno = ENOTSUP;
            goto failed;
        }
    } else if (!has_rate || fps(p.parm.capture.timeperframe) <= 0) {
        errno = ENOTSUP;
        goto failed;
    }
    // Read back both ioctls: drivers may adjust fields even after acknowledging a request.
    struct v4l2_format actual = {0};
    actual.type = c->source.type;
    step = "verify V4L2 input format";
    if (call(c->fd, VIDIOC_G_FMT, &actual) < 0)
        goto failed;
    if (actual.fmt.pix.width != wanted.width || actual.fmt.pix.height != wanted.height ||
        actual.fmt.pix.pixelformat != wanted.pixelformat) {
        errno = ENOTSUP;
        goto failed;
    }
    c->source = actual;
    struct v4l2_streamparm verified = {0};
    verified.type = c->source.type;
    if (call(c->fd, VIDIOC_G_PARM, &verified) == 0)
        p = verified;
    if (!p.parm.capture.timeperframe.numerator || !p.parm.capture.timeperframe.denominator ||
        p.parm.capture.timeperframe.numerator > INT_MAX ||
        p.parm.capture.timeperframe.denominator > INT_MAX ||
        (rate > 0 && fabs(fps(p.parm.capture.timeperframe) - rate) > 0.001)) {
        step = "verify V4L2 input frame rate";
        errno = ENOTSUP;
        goto failed;
    }
    c->interval = p.parm.capture.timeperframe;
    size_t width = c->source.fmt.pix.width, height = c->source.fmt.pix.height;
    if (!width || !height || width > 65535 || height > 65535 || width * height > FRAME_LIMIT / 4) {
        step = "V4L2 frame size exceeds memory limit";
        errno = EINVAL;
        goto failed;
    }
    if (strcmp(c->format, "mjpeg") && strcmp(c->format, "jpeg") && strcmp(c->format, "h264")) {
        c->converter = v4lconvert_create(c->fd);
        if (!c->converter) {
            step = "create V4L2 color converter";
            goto failed;
        }
        c->destination = c->source;
        c->destination.fmt.pix.pixelformat = V4L2_PIX_FMT_BGR24;
        c->destination.fmt.pix.bytesperline = width * 3;
        c->destination.fmt.pix.sizeimage = width * height * 3;
        c->rgb_size = width * height * 3;
        c->rgb = malloc(c->rgb_size);
        if (!c->rgb)
            goto failed;
    }
    struct v4l2_requestbuffers req = {0};
    req.count = 4;
    req.type = c->source.type;
    req.memory = V4L2_MEMORY_MMAP;
    step = "allocate V4L2 mmap buffers";
    if (call(c->fd, VIDIOC_REQBUFS, &req) < 0)
        goto failed;
    if (req.count < 1 || req.count > 64) {
        errno = ENOBUFS;
        goto failed;
    }
    c->count = req.count;
    c->buffers = calloc(req.count, sizeof(*c->buffers));
    if (!c->buffers)
        goto failed;
    for (unsigned i = 0; i < c->count; i++) {
        struct v4l2_buffer b = {0};
        b.type = c->source.type;
        b.memory = V4L2_MEMORY_MMAP;
        b.index = i;
        step = "map V4L2 input buffer";
        if (call(c->fd, VIDIOC_QUERYBUF, &b) < 0)
            goto failed;
        if (!b.length || b.length > FRAME_LIMIT) {
            errno = EINVAL;
            goto failed;
        }
        c->buffers[i].size = b.length;
        c->buffers[i].data =
            mmap(NULL, b.length, PROT_READ | PROT_WRITE, MAP_SHARED, c->fd, b.m.offset);
        if (c->buffers[i].data == MAP_FAILED)
            goto failed;
        if (call(c->fd, VIDIOC_QBUF, &b) < 0)
            goto failed;
    }
    enum v4l2_buf_type type = c->source.type;
    step = "start V4L2 input stream";
    if (call(c->fd, VIDIOC_STREAMON, &type) < 0)
        goto failed;
    c->streaming = 1;
    return c;
failed:
    fail(error, n, step);
    bk_v4l_close(c);
    return NULL;
}
int bk_v4l_read(bk_v4l_capture *c, int latest, uint8_t **data, size_t *size, int *index,
                char *error, size_t n) {
    int held = -1;
    struct v4l2_buffer current = {0};
    // Bound draining even if a driver replenishes its queue faster than the reader.
    for (unsigned i = 0; i < (latest ? c->count : 1); i++) {
        struct v4l2_buffer b = {0};
        b.type = c->source.type;
        b.memory = V4L2_MEMORY_MMAP;
        if (call(c->fd, VIDIOC_DQBUF, &b) < 0) {
            if (errno == EAGAIN)
                break;
            if (held >= 0)
                bk_v4l_done(c, held, error, n);
            fail(error, n, "dequeue V4L2 input");
            return -1;
        }
        if (b.index >= c->count || b.bytesused > c->buffers[b.index].size) {
            if (b.index < c->count)
                bk_v4l_done(c, b.index, error, n);
            if (held >= 0)
                bk_v4l_done(c, held, error, n);
            snprintf(error, n, "driver returned an invalid V4L2 buffer");
            return -1;
        }
        if (held >= 0 && bk_v4l_done(c, held, error, n) < 0) {
            bk_v4l_done(c, b.index, error, n);
            return -1;
        }
        held = b.index;
        current = b;
    }
    if (held < 0)
        return 0;
    *index = held;
    *size = current.bytesused;
    *data = c->buffers[held].data;
    return 1;
}
int bk_v4l_done(bk_v4l_capture *c, int index, char *error, size_t n) {
    struct v4l2_buffer b = {0};
    b.type = c->source.type;
    b.memory = V4L2_MEMORY_MMAP;
    b.index = index;
    if (call(c->fd, VIDIOC_QBUF, &b) < 0) {
        fail(error, n, "requeue V4L2 input");
        return -1;
    }
    return 0;
}
int bk_v4l_convert(bk_v4l_capture *c, uint8_t *src, size_t size, uint8_t *dst, size_t dst_size,
                   char *error, size_t n) {
    if (!c->converter || size > INT_MAX ||
        dst_size < (size_t)c->source.fmt.pix.width * c->source.fmt.pix.height * 4) {
        snprintf(error, n, "invalid V4L2 conversion buffers");
        return -1;
    }
    int got = v4lconvert_convert(c->converter, &c->source, &c->destination, src, (int)size, c->rgb,
                                 (int)c->rgb_size);
    if (got < 0 || (size_t)got != c->rgb_size) {
        snprintf(error, n, "V4L2 conversion from %s: %s", c->format,
                 v4lconvert_get_error_message(c->converter));
        return -1;
    }
    for (size_t i = 0, j = 0; i < c->rgb_size; i += 3, j += 4) {
        dst[j] = c->rgb[i];
        dst[j + 1] = c->rgb[i + 1];
        dst[j + 2] = c->rgb[i + 2];
        dst[j + 3] = 255;
    }
    return 0;
}
int bk_v4l_width(bk_v4l_capture *c) {
    return c->source.fmt.pix.width;
}
int bk_v4l_height(bk_v4l_capture *c) {
    return c->source.fmt.pix.height;
}
int bk_v4l_rate_num(bk_v4l_capture *c) {
    return c->interval.denominator;
}
int bk_v4l_rate_den(bk_v4l_capture *c) {
    return c->interval.numerator;
}
const char *bk_v4l_format(bk_v4l_capture *c) {
    return c->format;
}
void bk_v4l_keyframe(bk_v4l_capture *c) {
    struct v4l2_control control = {0};
    control.id = V4L2_CID_MPEG_VIDEO_FORCE_KEY_FRAME;
    control.value = 1;
    call(c->fd, VIDIOC_S_CTRL, &control);
}

// The caller owns the exported FD and must keep the capture buffer dequeued.
int bk_v4l_nv12(bk_v4l_capture *c, int index, size_t used, int *fd, unsigned *pitch,
                size_t *allocation) {
    *fd = -1;
    if (c->source.fmt.pix.quantization == V4L2_QUANTIZATION_FULL_RANGE)
        return 0;
    if (c->source.fmt.pix.pixelformat != V4L2_PIX_FMT_NV12 || index < 0 ||
        (unsigned)index >= c->count)
        return 0;
    unsigned w = c->source.fmt.pix.width, h = c->source.fmt.pix.height,
             p = c->source.fmt.pix.bytesperline;
    if (!p)
        p = w;
    if ((w | h) & 1 || p < w || p > 65536 || (size_t)p * h * 3 / 2 > used ||
        used > c->buffers[index].size)
        return 0;
    *pitch = p;
    *allocation = c->buffers[index].size;
    struct v4l2_exportbuffer ex = {0};
    ex.type = c->source.type;
    ex.index = index;
    ex.flags = O_CLOEXEC | O_RDONLY;
    if (call(c->fd, VIDIOC_EXPBUF, &ex) == 0)
        *fd = ex.fd;
    return 1;
}
