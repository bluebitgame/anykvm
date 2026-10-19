#ifndef ANYKVM_RDP_PROGRESSIVE_NATIVE_H
#define ANYKVM_RDP_PROGRESSIVE_NATIVE_H

#include <stddef.h>
#include <stdint.h>

/* Independently implemented from MS-RDPEGFX / the existing Go encoder. */
typedef struct {
    uint16_t x, y, width, height;
} anykvm_rfx_rect;

#define ANYKVM_RFX_MAX_TILES 8

enum { ANYKVM_RFX_BGRA = 0, ANYKVM_RFX_NV12 = 1 };

void *bk_rfx_open(char *message, size_t capacity);
enum {
    ANYKVM_RFX_SIMD_SCALAR = 0,
    ANYKVM_RFX_SIMD_BASELINE = 1,
    ANYKVM_RFX_SIMD_AVX2 = 2
};
/* A forced optional backend is rejected when CPU/OS support is absent. */
int bk_rfx_set_simd(void *context, int mode);
/* Uniform scalar quantization for all bands/components, 6..15; default is 6. */
int bk_rfx_set_quantization(void *context, int quantization);
void bk_rfx_disable_simd(void *context);
const char *bk_rfx_backend(void *context);
int bk_rfx_run(void *context, const uint8_t *pixels, size_t pixel_bytes,
               uint32_t width, uint32_t height, const anykvm_rfx_rect *rects,
               size_t count, uint32_t frame, int first, const uint8_t **output,
               size_t *output_bytes, char *message, size_t capacity);
/* NV12 is tightly packed Y + UV with ceil(width/2)*2 UV stride, BT.709 video range. */
int bk_rfx_run_format(void *context, const uint8_t *pixels, size_t pixel_bytes, int format,
               uint32_t width, uint32_t height, const anykvm_rfx_rect *rects,
               size_t count, uint32_t frame, int first, const uint8_t **output,
               size_t *output_bytes, char *message, size_t capacity);
void bk_rfx_close(void *context);

enum { ANYKVM_RFX_SIMPLE = 0, ANYKVM_RFX_FIRST = 1, ANYKVM_RFX_UPGRADE = 2 };
/* Synchronous, caller-owned full tile-grid cache: 3*4096 int32 coefficients
 * per tile, row-major. FIRST writes base quantization before LL differencing;
 * UPGRADE reads it without retaining any pointers. extra_bits is 1..4. */
int bk_rfx_run_pass(void *context, const uint8_t *pixels, size_t pixel_bytes, int format,
               uint32_t width, uint32_t height, const anykvm_rfx_rect *rects,
               size_t count, uint32_t frame, int first, int pass, unsigned extra_bits,
               int32_t *cache, size_t cache_count, const uint8_t **output,
               size_t *output_bytes, char *message, size_t capacity);

#endif
