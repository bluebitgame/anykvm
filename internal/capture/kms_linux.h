#ifndef BK_KMS_LINUX_H
#define BK_KMS_LINUX_H
#include <stddef.h>
#include <stdint.h>
typedef struct bk_kms bk_kms;
typedef struct {
    int fd;
    int64_t pts_us; /* Native CLOCK_MONOTONIC acquisition time, not a vblank/Present timestamp. */
    uint32_t planes, pitches[4], offsets[4];
    uint32_t width, height, fourcc, pitch, offset, fb_id;
    uint64_t modifier, size;
    uint32_t x, y, crop_width, crop_height;
    int rotation;
} bk_kms_frame;
typedef struct {
    char card[64];
    uint32_t connector, crtc, plane, width, height, rate_num, rate_den;
    uint32_t device_major, device_minor;
} bk_kms_output;
int bk_kms_list(bk_kms_output *, int, char *, size_t);
bk_kms *bk_kms_open(int, bk_kms_output *, char *, size_t);
int bk_kms_next(bk_kms *, bk_kms_frame *, char *, size_t);
void bk_kms_close(bk_kms *);
#endif
