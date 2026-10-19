#ifndef BK_GPUCONV_LINUX_H
#define BK_GPUCONV_LINUX_H
#include <stddef.h>
#include <stdint.h>
typedef struct bk_vk bk_vk;
typedef struct bk_vk_image bk_vk_image;
typedef struct {
    uint32_t planes, pitches[4], offsets[4];
    int fd;
    uint32_t width, height, fourcc, pitch, offset;
    uint64_t modifier, size;
    uint32_t x, y, crop_width, crop_height;
    int rotation;
} bk_vk_source;
enum { BK_VK_DESKTOP = 0, BK_VK_VIDEO = 1, BK_VK_JPEG = 2 };
bk_vk *bk_vk_open(uint32_t, uint32_t, const void *, size_t, int, char *, size_t);
void bk_vk_close(bk_vk *);
bk_vk_image *bk_vk_convert(bk_vk *, const bk_vk_source *, int, int, int, char *, size_t);
uint32_t bk_vk_image_pitch(bk_vk_image *);
int bk_vk_readback(bk_vk_image *, void **, size_t *, char *, size_t);
void bk_vk_image_free(bk_vk_image *);
int bk_vk_jpeg_init(bk_vk *, const void *, size_t, const void *, size_t, const void *, size_t, char *, size_t);
int bk_vk_jpeg_encode(bk_vk *, const bk_vk_source *, int, int, const uint32_t *, const uint32_t *,
    const uint8_t *, size_t, uint8_t **, size_t *, char *, size_t);
typedef struct bk_vk_encoder bk_vk_encoder;
bk_vk_encoder *bk_vk_encoder_new(bk_vk *, int, int, int, int, int, char *, size_t);
int bk_vk_encoder_encode(bk_vk_encoder *, const bk_vk_source *, const uint8_t *, size_t, int, uint8_t **, size_t *,
                         char *, size_t);
void bk_vk_encoder_free(bk_vk_encoder *);
#endif
