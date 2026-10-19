// Exercise the production backend with a deterministic V4L2 driver model.
// The fixture also runs on macOS when supplied Linux UAPI include directories.
#include <assert.h>
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <sys/time.h>
#include <time.h>
#include <unistd.h>
#include <linux/videodev2.h>

static int fake_open(const char *, int, ...);
static int fake_close(int);
static int fake_ioctl(int, unsigned long, void *);
static void *fake_mmap(void *, size_t, int, int, int, off_t);
static int fake_munmap(void *, size_t);
static int fake_poll(struct pollfd *, nfds_t, int);
static int fake_clock(clockid_t, struct timespec *);
#define open fake_open
#define close fake_close
#define ioctl fake_ioctl
#define mmap fake_mmap
#define munmap fake_munmap
#define poll fake_poll
#define clock_gettime fake_clock
#include "../v4l2_linux.c"
#undef open
#undef close
#undef ioctl
#undef mmap
#undef munmap
#undef poll
#undef clock_gettime

static struct {
    unsigned caps, capture_format;
    const char *driver;
    unsigned stride, rows;
    void *maps[8];
    unsigned map_count, unmap_count, closes, starts, stops, force_count;
    unsigned map_fail_at, stream_fail_at, rate_changed, input_pending, capture_stage;
    unsigned dq_input_count, timeout, bad_capture, bad_pts, failed_control, interrupt;
    struct timeval timestamp;
    unsigned frames;
    int64_t clock_ms;
} model;

static void reset(void) {
    memset(&model, 0, sizeof(model));
    model.caps = V4L2_CAP_VIDEO_M2M_MPLANE | V4L2_CAP_STREAMING;
    model.driver = "bcm2835-codec";
    model.capture_format = V4L2_PIX_FMT_H264;
    model.stride = 640; model.rows = 480;
}
static int again(void) { errno = EAGAIN; return -1; }
static int fake_open(const char *path, int flags, ...) {
    assert(!strcmp(path, "/dev/video42"));
    assert(flags & O_NONBLOCK);
    return 42;
}
static int fake_close(int fd) { assert(fd == 42); model.closes++; return 0; }
static void *fake_mmap(void *address, size_t size, int prot, int flags, int fd, off_t offset) {
    (void)address; (void)prot; (void)flags;
    assert(fd == 42 && offset / 4096 < 8);
    if (model.map_fail_at && model.map_count + 1 == model.map_fail_at) {
        errno = ENOMEM; return MAP_FAILED;
    }
    void *data = calloc(1, size);
    assert(data);
    model.maps[offset / 4096] = data;
    model.map_count++;
    return data;
}
static int fake_munmap(void *address, size_t size) {
    assert(address && size);
    free(address); model.unmap_count++; return 0;
}
static int fake_clock(clockid_t clock, struct timespec *time) {
    assert(clock == CLOCK_MONOTONIC);
    model.clock_ms += model.timeout ? 3000 : 1;
    time->tv_sec = model.clock_ms / 1000;
    time->tv_nsec = (model.clock_ms % 1000) * 1000000;
    return 0;
}
static int fake_poll(struct pollfd *fds, nfds_t count, int timeout) {
    assert(count == 1 && fds->fd == 42 && timeout > 0 && timeout <= 5000);
    if (model.interrupt) { model.interrupt = 0; errno = EINTR; return -1; }
    fds->revents = fds->events;
    return 1;
}
static int fake_ioctl(int fd, unsigned long request, void *arg) {
    assert(fd == 42);
    switch (request) {
    case VIDIOC_QUERYCAP: {
        struct v4l2_capability *cap = arg;
        snprintf((char *)cap->driver, sizeof(cap->driver), "%s", model.driver);
        // Global capabilities must not override the node-specific caps.
        cap->capabilities = V4L2_CAP_DEVICE_CAPS | V4L2_CAP_VIDEO_M2M_MPLANE | V4L2_CAP_STREAMING;
        cap->device_caps = model.caps;
        return 0;
    }
    case VIDIOC_ENUM_FMT: {
        struct v4l2_fmtdesc *format = arg;
        if (format->index) { errno = EINVAL; return -1; }
        format->pixelformat = format->type == BK_PI_CAPTURE ? model.capture_format : V4L2_PIX_FMT_NV12;
        return 0;
    }
    case VIDIOC_S_FMT: {
        struct v4l2_format *format = arg;
        if (format->type == BK_PI_OUTPUT) {
            assert(format->fmt.pix_mp.width == 638 && format->fmt.pix_mp.height == 478);
            assert(format->fmt.pix_mp.colorspace == V4L2_COLORSPACE_REC709);
            format->fmt.pix_mp.height = model.rows;
            format->fmt.pix_mp.plane_fmt[0].bytesperline = model.stride;
            format->fmt.pix_mp.plane_fmt[0].sizeimage = model.stride * model.rows * 3 / 2;
        }
        return 0;
    }
    case VIDIOC_S_SELECTION: {
        struct v4l2_selection *crop = arg;
        assert(crop->type == V4L2_BUF_TYPE_VIDEO_OUTPUT && crop->r.width == 638 && crop->r.height == 478);
        return 0;
    }
    case VIDIOC_S_PARM: {
        struct v4l2_streamparm *rate = arg;
        assert(rate->parm.output.timeperframe.numerator == 1001);
        assert(rate->parm.output.timeperframe.denominator == 30000);
        if (model.rate_changed) rate->parm.output.timeperframe.denominator = 30;
        return 0;
    }
    case VIDIOC_S_CTRL: {
        struct v4l2_control *control = arg;
        if (model.failed_control == control->id) { errno = EINVAL; return -1; }
        if (control->id == V4L2_CID_MPEG_VIDEO_FORCE_KEY_FRAME) model.force_count++;
        if (control->id == V4L2_CID_MPEG_VIDEO_B_FRAMES) assert(!control->value);
        if (control->id == V4L2_CID_MPEG_VIDEO_HEADER_MODE)
            assert(control->value == V4L2_MPEG_VIDEO_HEADER_MODE_JOINED_WITH_1ST_FRAME);
        return 0;
    }
    case VIDIOC_QUERYCTRL: return 0;
    case VIDIOC_REQBUFS: return 0;
    case VIDIOC_QUERYBUF: {
        struct v4l2_buffer *buffer = arg;
        buffer->m.planes[0].length = buffer->type == BK_PI_OUTPUT ? model.stride * model.rows * 3 / 2 : 2*1024*1024;
        buffer->m.planes[0].m.mem_offset = (buffer->index + (buffer->type == BK_PI_CAPTURE ? 4 : 0)) * 4096;
        return 0;
    }
    case VIDIOC_STREAMON:
        model.starts++;
        if (model.stream_fail_at == model.starts) { errno = EIO; return -1; }
        return 0;
    case VIDIOC_STREAMOFF: model.stops++; return 0;
    case VIDIOC_QBUF: {
        struct v4l2_buffer *buffer = arg;
        if (buffer->type == BK_PI_OUTPUT) {
            assert(!model.input_pending && buffer->index == 0);
            model.input_pending = 1; model.capture_stage = 0; model.dq_input_count = 0;
            uint8_t *raw = model.maps[0];
            // Verify visible luma/chroma and both horizontal and vertical padding.
            assert(raw[0] == 40 && raw[model.stride * 477 + 637] == 40);
            assert(raw[638] == 16);
            if (model.rows > 478) assert(raw[model.stride * 478] == 16);
            unsigned uv = model.stride * model.rows;
            assert(raw[uv] == 90 && raw[uv + model.stride * 238 + 637] == 90);
            assert(raw[uv + 638] == 128);
            if (model.rows > 478) assert(raw[uv + model.stride * 239] == 128);
            assert(buffer->m.planes[0].bytesused == model.stride * model.rows * 3 / 2);
            model.timestamp = buffer->timestamp;
            uint64_t pts = (uint64_t)buffer->timestamp.tv_sec * 1000000 + buffer->timestamp.tv_usec;
            assert(pts == 1 + (uint64_t)model.frames++ * 1001 * 1000000 / 30000);
        }
        return 0;
    }
    case VIDIOC_DQBUF: {
        struct v4l2_buffer *buffer = arg;
        if (model.timeout) return again();
        if (buffer->type == BK_PI_OUTPUT) {
            if (++model.dq_input_count < 3) return again();
            assert(model.input_pending);
            model.input_pending = 0;
            return 0;
        }
        if (model.capture_stage >= 2) return again();
        assert(model.input_pending);
        static const uint8_t header[] = {0, 0, 0, 1, 0x67, 0x42, 0, 0, 0, 1, 0x68, 0xce};
        static const uint8_t picture[] = {0, 0, 1, 0x65, 0x80};
        const uint8_t *packet = model.capture_stage ? picture : header;
        unsigned size = model.capture_stage ? sizeof(picture) : sizeof(header);
        unsigned index = model.capture_stage++;
        buffer->index = index;
        buffer->timestamp = model.timestamp;
        if (model.bad_pts) buffer->timestamp.tv_usec++;
        buffer->m.planes[0].data_offset = 5;
        buffer->m.planes[0].bytesused = size + 5;
        if (model.bad_capture) buffer->m.planes[0].bytesused = 3*1024*1024;
        memcpy((uint8_t *)model.maps[index + 4] + 5, packet, size);
        return 0;
    }
    default: assert(!"unexpected V4L2 ioctl"); return -1;
    }
}
static bk_pi_encoder *create(char *error, size_t size) {
    return bk_pi_open("/dev/video42", 638, 478, 30000, 1001, 2000, error, size);
}
static void cleanup_check(void) {
    assert(model.map_count == model.unmap_count && model.closes == 1);
}
int main(void) {
    char error[512] = {0};
    reset(); assert(bk_pi_candidate("/dev/video42"));
    reset(); model.caps = V4L2_CAP_VIDEO_CAPTURE | V4L2_CAP_STREAMING;
    assert(!bk_pi_candidate("/dev/video42"));
    reset(); model.capture_format = V4L2_PIX_FMT_NV12;
    assert(!bk_pi_candidate("/dev/video42"));
    reset(); model.driver = "vicodec";
    assert(!bk_pi_candidate("/dev/video42"));
    uint8_t *pixels = malloc(638*478*3/2);
    memset(pixels, 40, 638*478); memset(pixels + 638*478, 90, 638*478/2);
    for (unsigned rows = 478; rows <= 480; rows += 2) {
        reset(); model.rows = rows;
        bk_pi_encoder *e = create(error, sizeof(error)); assert(e);
        uint8_t *output; size_t size;
        for (unsigned frame = 0; frame < 12; frame++) {
            model.interrupt = 1;
            assert(!bk_pi_encode(e, pixels, 638*478*3/2, frame == 0 || frame == 6,
                                 &output, &size, error, sizeof(error)));
            assert(size == 17 && pi_has_picture(output, size)); free(output);
        }
        assert(model.starts == 2 && !model.stops && model.force_count == 2);
        assert(bk_pi_encode(e, pixels, 1, 0, &output, &size, error, sizeof(error)) < 0);
        bk_pi_close(e); cleanup_check(); assert(model.stops == 2);
    }
    for (unsigned failure = 0; failure < 6; failure++) {
        reset();
        if (failure == 0) model.map_fail_at = 2;
        if (failure == 1) model.stream_fail_at = 2;
        if (failure == 2) model.rate_changed = 1;
        if (failure == 3) model.failed_control = V4L2_CID_MPEG_VIDEO_B_FRAMES;
        if (failure == 4) model.stride = 600;
        if (failure == 5) model.caps = V4L2_CAP_VIDEO_CAPTURE;
        assert(!create(error, sizeof(error))); cleanup_check();
        if (failure == 1) assert(model.stops == 1);
    }
    for (unsigned failure = 0; failure < 3; failure++) {
        reset(); bk_pi_encoder *e = create(error, sizeof(error)); assert(e);
        if (failure == 0) model.timeout = 1;
        if (failure == 1) model.bad_capture = 1;
        if (failure == 2) model.bad_pts = 1;
        uint8_t *output; size_t size;
        assert(bk_pi_encode(e, pixels, 638*478*3/2, 1, &output, &size, error, sizeof(error)) < 0);
        assert(!output && !size);
        bk_pi_close(e); cleanup_check();
    }
    free(pixels);
    puts("native V4L2 fixture passed");
    return 0;
}
