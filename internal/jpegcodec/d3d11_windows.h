#ifndef ANYKVM_JPEG_D3D11_H
#define ANYKVM_JPEG_D3D11_H
#include <stddef.h>
#include <stdint.h>
typedef struct bk_jpeg_gpu bk_jpeg_gpu;
typedef struct {
    const uint32_t *tiles;
    size_t count;
    uint32_t source_width, source_height;
} bk_jpeg_damage;
bk_jpeg_gpu *bk_jpeg_gpu_open(uint64_t luid, const char *shader, size_t length, char *error, size_t capacity);
int bk_jpeg_gpu_encode(bk_jpeg_gpu *, uintptr_t, int, int, int,
    const uint32_t *, const uint32_t *, const uint8_t *, size_t, uint8_t **, size_t *, char *, size_t);
int bk_jpeg_gpu_encode_mode(bk_jpeg_gpu *, uintptr_t, int, int, int,
    const uint32_t *, const uint32_t *, const uint8_t *, size_t, int, uint8_t **, size_t *, char *, size_t);
int bk_jpeg_gpu_encode_damage(bk_jpeg_gpu *, uintptr_t, int, int, int,
    const uint32_t *, const uint32_t *, const uint8_t *, size_t, int, const bk_jpeg_damage *,
    uint8_t **, size_t *, char *, size_t);
void bk_jpeg_gpu_close(bk_jpeg_gpu *);

typedef struct bk_jpeg_decoder bk_jpeg_decoder;
bk_jpeg_decoder *bk_jpeg_decoder_open(const char *,size_t,char *,size_t);
int bk_jpeg_decoder_decode(bk_jpeg_decoder *,const uint8_t *,size_t,const uint32_t *,size_t,uint8_t *,size_t,char *,size_t);
void bk_jpeg_decoder_close(bk_jpeg_decoder *);

#endif
