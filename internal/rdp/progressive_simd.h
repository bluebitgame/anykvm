#ifndef ANYKVM_RDP_PROGRESSIVE_SIMD_H
#define ANYKVM_RDP_PROGRESSIVE_SIMD_H

#include <stdint.h>
#include <stddef.h>

/* Keep optional instructions confined to functions compiled with target(avx2).
 * The ordinary C translation units retain the x86-64/SSE2 baseline. */
#if (defined(__x86_64__) || defined(_M_X64)) && (defined(__GNUC__) || defined(__clang__))
#define ANYKVM_RFX_AVX2 1
#define ANYKVM_RFX_TARGET_AVX2 __attribute__((target("avx2")))
size_t bk_rfx_zero_run_avx2(const int32_t *coeffs, size_t count);
void bk_rfx_ict_avx2(int32_t planes[3][4096], const uint8_t *pixels,
                     unsigned width, unsigned height, unsigned x, unsigned y);
void bk_rfx_ict_nv12_avx2(int32_t planes[3][4096], const uint8_t *pixels,
                     unsigned width, unsigned height, unsigned x, unsigned y);
void bk_rfx_dwt_avx2(const int32_t *input, int32_t *vertical, int32_t *bands, int32_t *coeffs,
                      unsigned quantization);
void bk_rfx_dwt_reduce_avx2(const int32_t *input, int32_t *vertical, int32_t *bands, int32_t *coeffs,
                      unsigned quantization, int simd);
#endif

#endif
