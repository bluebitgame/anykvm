#ifndef BK_JPEG_SIMD_H
#define BK_JPEG_SIMD_H
#include <stdint.h>
#include <stddef.h>
enum { BK_JPEG_SSE2 = 1, BK_JPEG_AVX2 = 2, BK_JPEG_NEON = 3 };
typedef struct bk_jpeg_encoder bk_jpeg_encoder;
bk_jpeg_encoder *bk_jpeg_encoder_new(void);
void bk_jpeg_encoder_free(bk_jpeg_encoder *encoder);
int bk_jpeg_encoder_encode(bk_jpeg_encoder *encoder, const uint8_t *pixels, int width, int height, int level,
    const uint32_t *quantization, const uint32_t *huffman, const uint8_t *header, size_t header_bytes,
    uint8_t **jpeg, size_t *jpeg_bytes);
// huffman contains 1024 standard codes and 2048 combined small-AC entries.
int bk_jpeg_simd_encode(const uint8_t *pixels, int width, int height, int level,
    const uint32_t *quantization, const uint32_t *huffman, const uint8_t *header, size_t header_bytes,
    uint8_t **jpeg, size_t *jpeg_bytes);
int bk_jpeg_simd_decode(const uint32_t *metadata, const uint8_t *entropy, int level, uint8_t *pixels);
#endif
