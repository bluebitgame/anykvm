//go:build linux && cgo

#include "v4l2_linux.h"
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

#define BK_PI_BUFFERS 4
#define BK_PI_MAX_PACKET (32u * 1024u * 1024u)
#define BK_PI_OUTPUT V4L2_BUF_TYPE_VIDEO_OUTPUT_MPLANE
#define BK_PI_CAPTURE V4L2_BUF_TYPE_VIDEO_CAPTURE_MPLANE

struct bk_pi_buffer {
    void *data;
    size_t size;
};
struct bk_pi_queue {
    enum v4l2_buf_type type;
    struct bk_pi_buffer buffers[BK_PI_BUFFERS];
    unsigned count;
    int streaming;
    int dma;
};
struct bk_pi_encoder {
    int fd, width, height, fps_num, fps_den;
    unsigned stride, rows, input_size;
    uint64_t pts, pts_remainder;
    struct bk_pi_queue input, capture;
};

static int pi_ioctl(int fd, unsigned long request, void *arg) {
    int result;
    do {
        result = ioctl(fd, request, arg);
    } while (result < 0 && errno == EINTR);
    return result;
}
static int pi_error(char *error, size_t size, const char *operation) {
    snprintf(error, size, "%s: %s", operation, strerror(errno));
    return -1;
}
static int pi_message(char *error, size_t size, const char *message) {
    snprintf(error, size, "%s", message);
    return -1;
}
static int pi_format_supported(int fd, enum v4l2_buf_type type, unsigned format) {
    struct v4l2_fmtdesc f = {0};
    f.type = type;
    while (pi_ioctl(fd, VIDIOC_ENUM_FMT, &f) == 0) {
        if (f.pixelformat == format)
            return 1;
        f.index++;
    }
    return 0;
}
static int pi_device(int fd) {
    struct v4l2_capability cap = {0};
    if (pi_ioctl(fd, VIDIOC_QUERYCAP, &cap) < 0)
        return 0;
    unsigned caps = cap.capabilities & V4L2_CAP_DEVICE_CAPS ? cap.device_caps : cap.capabilities;
    // Restrict automatic selection to the Raspberry Pi codec driver. Camera,
    // decoder, ISP and software M2M nodes must never be selected as encoders.
    return !strncmp((const char *)cap.driver, "bcm2835-codec", sizeof(cap.driver)) &&
           (caps & V4L2_CAP_VIDEO_M2M_MPLANE) && (caps & V4L2_CAP_STREAMING) &&
           pi_format_supported(fd, BK_PI_CAPTURE, V4L2_PIX_FMT_H264) &&
           pi_format_supported(fd, BK_PI_OUTPUT, V4L2_PIX_FMT_NV12);
}
int bk_pi_candidate(const char *path) {
    int fd = open(path, O_RDWR | O_NONBLOCK | O_CLOEXEC);
    if (fd < 0)
        return 0;
    int result = pi_device(fd);
    close(fd);
    return result;
}
static int pi_control(int fd, unsigned id, int value, char *error, size_t size) {
    struct v4l2_control control = {.id = id, .value = value};
    if (pi_ioctl(fd, VIDIOC_S_CTRL, &control) == 0)
        return 0;
    snprintf(error, size, "V4L2 control 0x%x=%d: %s", id, value, strerror(errno));
    return -1;
}
static void pi_buffer(struct v4l2_buffer *buffer, struct v4l2_plane *plane, enum v4l2_buf_type type,
                      unsigned index) {
    memset(buffer, 0, sizeof(*buffer));
    memset(plane, 0, sizeof(*plane));
    buffer->type = type;
    buffer->memory = V4L2_MEMORY_MMAP;
    buffer->index = index;
    buffer->length = 1;
    buffer->m.planes = plane;
}
static int pi_allocate(bk_pi_encoder *e, struct bk_pi_queue *queue, char *error, size_t size) {
    struct v4l2_requestbuffers request = {0};
    request.type = queue->type;
    request.memory = queue->dma ? V4L2_MEMORY_DMABUF : V4L2_MEMORY_MMAP;
    request.count = BK_PI_BUFFERS;
    if (pi_ioctl(e->fd, VIDIOC_REQBUFS, &request) < 0)
        return pi_error(error, size, "VIDIOC_REQBUFS");
    if (!request.count || request.count > BK_PI_BUFFERS)
        return pi_message(error, size, "unsupported V4L2 buffer count");
    queue->count = request.count;
    if (queue->dma)
        return 0;
    for (unsigned i = 0; i < queue->count; i++) {
        struct v4l2_buffer buffer;
        struct v4l2_plane plane;
        pi_buffer(&buffer, &plane, queue->type, i);
        if (pi_ioctl(e->fd, VIDIOC_QUERYBUF, &buffer) < 0)
            return pi_error(error, size, "VIDIOC_QUERYBUF");
        if (buffer.length != 1 || !plane.length || plane.length > BK_PI_MAX_PACKET ||
            (queue->type == BK_PI_OUTPUT && plane.length < e->input_size))
            return pi_message(error, size, "invalid V4L2 mapped buffer size");
        void *data =
            mmap(NULL, plane.length, PROT_READ | PROT_WRITE, MAP_SHARED, e->fd, plane.m.mem_offset);
        if (data == MAP_FAILED)
            return pi_error(error, size, "mmap");
        queue->buffers[i].data = data;
        queue->buffers[i].size = plane.length;
    }
    return 0;
}
static int pi_capture_queue(bk_pi_encoder *e, unsigned index, char *error, size_t size) {
    struct v4l2_buffer buffer;
    struct v4l2_plane plane;
    pi_buffer(&buffer, &plane, BK_PI_CAPTURE, index);
    plane.length = e->capture.buffers[index].size;
    if (pi_ioctl(e->fd, VIDIOC_QBUF, &buffer) < 0)
        return pi_error(error, size, "queue compressed buffer");
    return 0;
}
void bk_pi_close(bk_pi_encoder *e) {
    if (!e)
        return;
    struct bk_pi_queue *queues[] = {&e->input, &e->capture};
    for (unsigned q = 0; q < 2; q++) {
        struct bk_pi_queue *queue = queues[q];
        if (e->fd >= 0 && queue->streaming)
            pi_ioctl(e->fd, VIDIOC_STREAMOFF, &queue->type);
    }
    // Closing the device also cancels buffers after a failed STREAMOFF.
    if (e->fd >= 0)
        close(e->fd);
    for (unsigned q = 0; q < 2; q++) {
        for (unsigned i = 0; i < queues[q]->count; i++) {
            if (queues[q]->buffers[i].data)
                munmap(queues[q]->buffers[i].data, queues[q]->buffers[i].size);
        }
    }
    free(e);
}
static bk_pi_encoder *pi_open(const char *path, int width, int height, int fps_num, int fps_den,
                              int bitrate, unsigned dma_pitch, char *error, size_t error_size) {
    if (width < 32 || height < 32 || width > 1920 || height > 1920 || width % 2 || height % 2 ||
        fps_num < 1 || fps_den < 1 || bitrate < 25 || bitrate > 25000) {
        pi_message(error, error_size,
                   "unsupported Raspberry Pi size, rate or bitrate (25..25000 kbps)");
        return NULL;
    }
    bk_pi_encoder *e = calloc(1, sizeof(*e));
    if (!e) {
        pi_error(error, error_size, "allocate encoder");
        return NULL;
    }
    e->fd = open(path, O_RDWR | O_NONBLOCK | O_CLOEXEC);
    e->input.type = BK_PI_OUTPUT;
    e->input.dma = dma_pitch != 0;
    e->capture.type = BK_PI_CAPTURE;
    e->width = width;
    e->height = height;
    e->fps_num = fps_num;
    e->fps_den = fps_den;
    if (e->fd < 0) {
        pi_error(error, error_size, "open codec device");
        goto fail;
    }
    if (!pi_device(e->fd)) {
        pi_message(error, error_size, "not a Raspberry Pi NV12/H.264 M2M encoder");
        goto fail;
    }
    struct v4l2_format format = {0};
    format.type = BK_PI_CAPTURE;
    format.fmt.pix_mp.pixelformat = V4L2_PIX_FMT_H264;
    format.fmt.pix_mp.width = width;
    format.fmt.pix_mp.height = height;
    format.fmt.pix_mp.field = V4L2_FIELD_NONE;
    format.fmt.pix_mp.num_planes = 1;
    // Leave space for large recovery pictures even at the maximum bitrate.
    format.fmt.pix_mp.plane_fmt[0].sizeimage = 2 * 1024 * 1024;
    if (pi_ioctl(e->fd, VIDIOC_S_FMT, &format) < 0) {
        pi_error(error, error_size, "set H.264 capture format");
        goto fail;
    }
    if (format.fmt.pix_mp.pixelformat != V4L2_PIX_FMT_H264 || format.fmt.pix_mp.num_planes != 1) {
        pi_message(error, error_size, "driver rejected H.264 capture format");
        goto fail;
    }
    memset(&format, 0, sizeof(format));
    format.type = BK_PI_OUTPUT;
    format.fmt.pix_mp.pixelformat = V4L2_PIX_FMT_NV12;
    format.fmt.pix_mp.width = width;
    format.fmt.pix_mp.height = height;
    format.fmt.pix_mp.field = V4L2_FIELD_NONE;
    format.fmt.pix_mp.plane_fmt[0].bytesperline = dma_pitch;
    format.fmt.pix_mp.colorspace = V4L2_COLORSPACE_REC709;
    format.fmt.pix_mp.ycbcr_enc = V4L2_YCBCR_ENC_709;
    format.fmt.pix_mp.quantization = V4L2_QUANTIZATION_LIM_RANGE;
    format.fmt.pix_mp.xfer_func = V4L2_XFER_FUNC_709;
    format.fmt.pix_mp.num_planes = 1;
    if (pi_ioctl(e->fd, VIDIOC_S_FMT, &format) < 0) {
        pi_error(error, error_size, "set NV12 output format");
        goto fail;
    }
    e->stride = format.fmt.pix_mp.plane_fmt[0].bytesperline;
    e->rows = format.fmt.pix_mp.height;
    e->input_size = format.fmt.pix_mp.plane_fmt[0].sizeimage;
    if (format.fmt.pix_mp.pixelformat != V4L2_PIX_FMT_NV12 || format.fmt.pix_mp.num_planes != 1 ||
        format.fmt.pix_mp.width != (unsigned)width || e->stride < (unsigned)width ||
        e->stride > 8192 || e->rows < (unsigned)height || e->rows > 8192 || e->rows % 2 ||
        e->input_size < (size_t)e->stride * e->rows * 3 / 2 || e->input_size > BK_PI_MAX_PACKET) {
        pi_message(error, error_size, "driver returned an unsupported NV12 layout");
        goto fail;
    }
    if (dma_pitch && (e->stride != dma_pitch || e->rows != (unsigned)height)) {
        pi_message(error, error_size, "DMA-BUF pitch/height differ from encoder layout");
        goto fail;
    }
    struct v4l2_selection crop = {0};
    crop.type = V4L2_BUF_TYPE_VIDEO_OUTPUT;
    crop.target = V4L2_SEL_TGT_CROP;
    crop.r.width = width;
    crop.r.height = height;
    if (pi_ioctl(e->fd, VIDIOC_S_SELECTION, &crop) < 0 || crop.r.left || crop.r.top ||
        crop.r.width != (unsigned)width || crop.r.height != (unsigned)height) {
        pi_message(error, error_size, "driver rejected the visible picture crop");
        goto fail;
    }
    struct v4l2_streamparm rate = {0};
    rate.type = BK_PI_OUTPUT;
    rate.parm.output.timeperframe.numerator = fps_den;
    rate.parm.output.timeperframe.denominator = fps_num;
    if (pi_ioctl(e->fd, VIDIOC_S_PARM, &rate) < 0) {
        pi_error(error, error_size, "set encoder frame rate");
        goto fail;
    }
    if ((uint64_t)rate.parm.output.timeperframe.numerator * fps_num !=
            (uint64_t)rate.parm.output.timeperframe.denominator * fps_den ||
        !rate.parm.output.timeperframe.denominator) {
        pi_message(error, error_size, "driver changed the requested frame rate");
        goto fail;
    }
    unsigned blocks = ((width + 15) / 16) * ((height + 15) / 16);
    int level = bitrate <= 20000 && blocks <= 8192 &&
                        (uint64_t)blocks * fps_num <= (uint64_t)245760 * fps_den
                    ? V4L2_MPEG_VIDEO_H264_LEVEL_4_0
                : blocks <= 8704 && (uint64_t)blocks * fps_num <= (uint64_t)522240 * fps_den
                    ? V4L2_MPEG_VIDEO_H264_LEVEL_4_2
                    : V4L2_MPEG_VIDEO_H264_LEVEL_5_1;
    const struct {
        unsigned id;
        int value;
    } controls[] = {
        {V4L2_CID_MPEG_VIDEO_BITRATE_MODE, V4L2_MPEG_VIDEO_BITRATE_MODE_CBR},
        {V4L2_CID_MPEG_VIDEO_BITRATE, bitrate * 1000},
        {V4L2_CID_MPEG_VIDEO_H264_PROFILE, V4L2_MPEG_VIDEO_H264_PROFILE_CONSTRAINED_BASELINE},
        {V4L2_CID_MPEG_VIDEO_H264_LEVEL, level},
        {V4L2_CID_MPEG_VIDEO_B_FRAMES, 0},
        {V4L2_CID_MPEG_VIDEO_H264_I_PERIOD, (int)(((int64_t)fps_num * 2 + fps_den - 1) / fps_den)},
        {V4L2_CID_MPEG_VIDEO_HEADER_MODE, V4L2_MPEG_VIDEO_HEADER_MODE_JOINED_WITH_1ST_FRAME},
        {V4L2_CID_MPEG_VIDEO_REPEAT_SEQ_HEADER, 1},
    };
    for (unsigned i = 0; i < sizeof(controls) / sizeof(controls[0]); i++) {
        if (pi_control(e->fd, controls[i].id, controls[i].value, error, error_size) < 0)
            goto fail;
    }
    struct v4l2_queryctrl force = {.id = V4L2_CID_MPEG_VIDEO_FORCE_KEY_FRAME};
    if (pi_ioctl(e->fd, VIDIOC_QUERYCTRL, &force) < 0 || force.flags & V4L2_CTRL_FLAG_DISABLED) {
        pi_message(error, error_size, "driver has no recovery-frame control");
        goto fail;
    }
    if (pi_allocate(e, &e->input, error, error_size) < 0 ||
        pi_allocate(e, &e->capture, error, error_size) < 0)
        goto fail;
    for (unsigned i = 0; i < e->capture.count; i++) {
        if (pi_capture_queue(e, i, error, error_size) < 0)
            goto fail;
    }
    struct bk_pi_queue *queues[] = {&e->capture, &e->input};
    for (unsigned i = 0; i < 2; i++) {
        if (pi_ioctl(e->fd, VIDIOC_STREAMON, &queues[i]->type) < 0) {
            pi_error(error, error_size, "VIDIOC_STREAMON");
            goto fail;
        }
        queues[i]->streaming = 1;
    }
    return e;
fail:
    bk_pi_close(e);
    return NULL;
}

static int64_t pi_milliseconds(void) {
    struct timespec now;
    if (clock_gettime(CLOCK_MONOTONIC, &now) < 0)
        return -1;
    return (int64_t)now.tv_sec * 1000 + now.tv_nsec / 1000000;
}
static int pi_has_picture(const uint8_t *data, size_t size) {
    for (size_t i = 0; i + 3 < size; i++) {
        if (!data[i] && !data[i + 1] && data[i + 2] == 1) {
            unsigned type = data[i + 3] & 31;
            if (type == 1 || type == 5)
                return 1;
        }
    }
    return 0;
}
static int pi_encode(bk_pi_encoder *e, const uint8_t *nv12, size_t input_size, int dma_fd,
                     int force_idr, uint8_t **output, size_t *output_size, char *error,
                     size_t error_size) {
    *output = NULL;
    *output_size = 0;
    if ((!e->input.dma && (!nv12 || input_size != (size_t)e->width * e->height * 3 / 2)) ||
        (e->input.dma && (dma_fd < 0 || input_size < e->input_size)))
        return pi_message(error, error_size, "invalid NV12 input size");
    if (force_idr &&
        pi_control(e->fd, V4L2_CID_MPEG_VIDEO_FORCE_KEY_FRAME, 0, error, error_size) < 0)
        return -1;
    if (!e->input.dma) {
        uint8_t *raw = e->input.buffers[0].data;
        size_t y_size = (size_t)e->stride * e->rows;
        memset(raw, 16, y_size);
        memset(raw + y_size, 128, e->input_size - y_size);
        for (int y = 0; y < e->height; y++)
            memcpy(raw + (size_t)y * e->stride, nv12 + (size_t)y * e->width, e->width);
        for (int y = 0; y < e->height / 2; y++)
            memcpy(raw + y_size + (size_t)y * e->stride,
                   nv12 + (size_t)e->width * e->height + (size_t)y * e->width, e->width);
    }
    // Keep a single submitted picture outstanding. Both queues must complete
    // before reusing input storage; no per-frame drain or stream restart occurs.
    // Accumulate a rational duration without multiplying a growing frame
    // count, which could overflow for large valid rational denominators.
    uint64_t pts = 1 + e->pts;
    e->pts_remainder += (uint64_t)e->fps_den * 1000000;
    e->pts += e->pts_remainder / e->fps_num;
    e->pts_remainder %= e->fps_num;
    struct v4l2_buffer buffer;
    struct v4l2_plane plane;
    pi_buffer(&buffer, &plane, BK_PI_OUTPUT, 0);
    plane.length = e->input.dma ? input_size : e->input.buffers[0].size;
    if (e->input.dma) {
        buffer.memory = V4L2_MEMORY_DMABUF;
        plane.m.fd = dma_fd;
    }
    plane.bytesused = e->input_size;
    buffer.field = V4L2_FIELD_NONE;
    buffer.timestamp.tv_sec = pts / 1000000;
    buffer.timestamp.tv_usec = pts % 1000000;
    if (pi_ioctl(e->fd, VIDIOC_QBUF, &buffer) < 0)
        return pi_error(error, error_size, "queue NV12 input");
    int input_done = 0, picture_done = 0;
    int64_t now = pi_milliseconds();
    if (now < 0)
        return pi_error(error, error_size, "clock_gettime");
    int64_t deadline = now + 5000;
    while (!input_done || !picture_done) {
        now = pi_milliseconds();
        if (now < 0) {
            pi_error(error, error_size, "clock_gettime");
            goto fail;
        }
        if (now >= deadline) {
            pi_message(error, error_size, "V4L2 encode timed out after 5 seconds");
            goto fail;
        }
        if (!input_done) {
            pi_buffer(&buffer, &plane, BK_PI_OUTPUT, 0);
            if (e->input.dma)
                buffer.memory = V4L2_MEMORY_DMABUF;
            if (pi_ioctl(e->fd, VIDIOC_DQBUF, &buffer) == 0) {
                if (buffer.index != 0 || buffer.flags & V4L2_BUF_FLAG_ERROR) {
                    pi_message(error, error_size, "invalid completed input buffer");
                    goto fail;
                }
                input_done = 1;
            } else if (errno != EAGAIN) {
                pi_error(error, error_size, "dequeue input");
                goto fail;
            }
        }
        if (!picture_done) {
            pi_buffer(&buffer, &plane, BK_PI_CAPTURE, 0);
            if (pi_ioctl(e->fd, VIDIOC_DQBUF, &buffer) == 0) {
                if (buffer.index >= e->capture.count || buffer.length != 1 ||
                    buffer.flags & (V4L2_BUF_FLAG_ERROR | V4L2_BUF_FLAG_LAST) ||
                    plane.data_offset > plane.bytesused ||
                    plane.bytesused > e->capture.buffers[buffer.index].size) {
                    pi_message(error, error_size, "invalid encoded capture buffer");
                    goto fail;
                }
                size_t bytes = plane.bytesused - plane.data_offset;
                const uint8_t *data =
                    (uint8_t *)e->capture.buffers[buffer.index].data + plane.data_offset;
                if (bytes > BK_PI_MAX_PACKET - *output_size) {
                    pi_message(error, error_size, "encoded picture is too large");
                    goto fail;
                }
                if (bytes) {
                    uint8_t *grown = realloc(*output, *output_size + bytes);
                    if (!grown) {
                        pi_error(error, error_size, "allocate encoded picture");
                        goto fail;
                    }
                    *output = grown;
                    memcpy(*output + *output_size, data, bytes);
                    *output_size += bytes;
                    picture_done = pi_has_picture(data, bytes);
                    if (picture_done &&
                        ((uint64_t)buffer.timestamp.tv_sec * 1000000 + buffer.timestamp.tv_usec !=
                         pts)) {
                        pi_message(error, error_size, "encoder reordered the submitted picture");
                        goto fail;
                    }
                }
                if (pi_capture_queue(e, buffer.index, error, error_size) < 0)
                    goto fail;
                // A separate header buffer can precede the first access unit.
                // Try dequeue again before sleeping on the device.
                if (!picture_done)
                    continue;
            } else if (errno != EAGAIN) {
                pi_error(error, error_size, "dequeue capture");
                goto fail;
            }
        }
        if (input_done && picture_done)
            return 0;
        now = pi_milliseconds();
        if (now < 0) {
            pi_error(error, error_size, "clock_gettime");
            goto fail;
        }
        if (now >= deadline) {
            pi_message(error, error_size, "V4L2 encode timed out after 5 seconds");
            goto fail;
        }
        struct pollfd wait = {.fd = e->fd,
                              .events = (picture_done ? 0 : POLLIN) | (input_done ? 0 : POLLOUT)};
        int result = poll(&wait, 1, (int)(deadline - now));
        if (result < 0 && errno != EINTR) {
            pi_error(error, error_size, "poll encoder");
            goto fail;
        }
        if (result > 0 && wait.revents & (POLLERR | POLLHUP | POLLNVAL)) {
            pi_message(error, error_size, "V4L2 encoder device stopped");
            goto fail;
        }
    }
    return 0;
fail:
    free(*output);
    *output = NULL;
    *output_size = 0;
    return -1;
}

bk_pi_encoder *bk_pi_open(const char *path, int w, int h, int num, int den, int bitrate,
                          char *error, size_t n) {
    return pi_open(path, w, h, num, den, bitrate, 0, error, n);
}
bk_pi_encoder *bk_pi_open_dma(const char *path, int w, int h, int num, int den, int bitrate,
                              unsigned pitch, char *error, size_t n) {
    return pi_open(path, w, h, num, den, bitrate, pitch, error, n);
}
int bk_pi_encode(bk_pi_encoder *e, const uint8_t *data, size_t size, int force, uint8_t **out,
                 size_t *n, char *error, size_t cap) {
    return pi_encode(e, data, size, -1, force, out, n, error, cap);
}
int bk_pi_encode_dma(bk_pi_encoder *e, int fd, size_t size, int force, uint8_t **out, size_t *n,
                     char *error, size_t cap) {
    if (size < e->input_size) {
        snprintf(error, cap, "capture allocation is smaller than encoder input");
        return 1;
    }
    return pi_encode(e, NULL, size, fd, force, out, n, error, cap);
}
