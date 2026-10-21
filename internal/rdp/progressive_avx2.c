//go:build cgo && (darwin || linux || windows)

// Original eight-lane implementations of the same fixed-point equations as
// progressive.go / progressive_native.c. No external codec code is used.
#include "progressive_simd.h"

#if defined(ANYKVM_RFX_AVX2)
#include <immintrin.h>
#include <string.h>

ANYKVM_RFX_TARGET_AVX2 size_t bk_rfx_zero_run_avx2(const int32_t *coeffs, size_t count) {
    size_t i = 0;
    for (; i + 8 <= count; i += 8) {
        __m256i values = _mm256_loadu_si256((const __m256i *)(coeffs + i));
        __m256i equal = _mm256_cmpeq_epi32(values, _mm256_setzero_si256());
        unsigned mask = (unsigned)_mm256_movemask_ps(_mm256_castsi256_ps(equal));
        if (mask != 255) return i + (unsigned)__builtin_ctz((~mask) & 255);
    }
    while (i < count && coeffs[i] == 0) i++;
    return i;
}

static inline ANYKVM_RFX_TARGET_AVX2 __m256i load8(const int32_t *p) {
    return _mm256_loadu_si256((const __m256i *)p);
}
static inline ANYKVM_RFX_TARGET_AVX2 void store8(int32_t *p, __m256i v) {
    _mm256_storeu_si256((__m256i *)p, v);
}
static inline ANYKVM_RFX_TARGET_AVX2 void even_odd8(const int32_t *p, __m256i *even, __m256i *odd) {
    __m256i a = load8(p), b = load8(p + 8);
    // Unpack is lane-local: reorder the four 64-bit groups to restore order.
    *even = _mm256_permute4x64_epi64(_mm256_unpacklo_epi64(
        _mm256_shuffle_epi32(a, 0x88), _mm256_shuffle_epi32(b, 0x88)), 0xd8);
    *odd = _mm256_permute4x64_epi64(_mm256_unpacklo_epi64(
        _mm256_shuffle_epi32(a, 0xdd), _mm256_shuffle_epi32(b, 0xdd)), 0xd8);
}
static inline ANYKVM_RFX_TARGET_AVX2 __m256i next8(__m256i v, int32_t next) {
    __m256i shifted = _mm256_permutevar8x32_epi32(v, _mm256_setr_epi32(1,2,3,4,5,6,7,7));
    return _mm256_blend_epi32(shifted, _mm256_set1_epi32(next), 0x80);
}
static inline ANYKVM_RFX_TARGET_AVX2 __m256i previous8(__m256i v, int32_t previous) {
    __m256i shifted = _mm256_permutevar8x32_epi32(v, _mm256_setr_epi32(0,0,1,2,3,4,5,6));
    return _mm256_blend_epi32(shifted, _mm256_set1_epi32(previous), 0x01);
}
static inline ANYKVM_RFX_TARGET_AVX2 void dwt_line8(const int32_t *input, int32_t *low, int32_t *high, unsigned size) {
    unsigned n = size / 2;
    for (unsigned i = 0; i < n; i += 8) {
        __m256i even, odd;
        even_odd8(input + 2 * i, &even, &odd);
        unsigned right = 2 * i + 16 < size ? 2 * i + 16 : size - 2;
        __m256i value = _mm256_sub_epi32(_mm256_sub_epi32(_mm256_add_epi32(odd, odd), even), next8(even, input[right]));
        store8(high + i, _mm256_srai_epi32(_mm256_add_epi32(value, _mm256_set1_epi32(2)), 2));
    }
    for (unsigned i = 0; i < n; i += 8) {
        __m256i even, odd;
        even_odd8(input + 2 * i, &even, &odd);
        __m256i current = load8(high + i);
        __m256i sum = _mm256_add_epi32(previous8(current, high[i ? i - 1 : 0]), current);
        store8(low + i, _mm256_add_epi32(even, _mm256_srai_epi32(_mm256_add_epi32(sum, _mm256_set1_epi32(1)), 1)));
    }
}

#define ANYKVM_REDUCE_TARGET ANYKVM_RFX_TARGET_AVX2
#define ANYKVM_REDUCE_LINE dwt_reduce_line8
#define ANYKVM_REDUCE_DWT bk_rfx_dwt_reduce_avx2
#define ANYKVM_REDUCE_LANES 8
#define ANYKVM_REDUCE_VEC __m256i
#define ANYKVM_REDUCE_LOAD load8
#define ANYKVM_REDUCE_STORE store8
#define ANYKVM_REDUCE_CONST _mm256_set1_epi32
#define ANYKVM_REDUCE_ADD _mm256_add_epi32
#define ANYKVM_REDUCE_SUB _mm256_sub_epi32
#define ANYKVM_REDUCE_SHIFT _mm256_srai_epi32
#define ANYKVM_REDUCE_QUANT_SHIFT(v,n) _mm256_sra_epi32(v,_mm_cvtsi32_si128((int)n))
#define ANYKVM_REDUCE_EVEN_ODD even_odd8
#define ANYKVM_REDUCE_NEXT next8
#define ANYKVM_REDUCE_PREVIOUS previous8
#include "progressive_reduce.h"

ANYKVM_RFX_TARGET_AVX2 void bk_rfx_dwt_avx2(const int32_t *input, int32_t *vertical, int32_t *bands, int32_t *coeffs,
                                         unsigned quantization) {
    int32_t low[32], high[32];
    size_t offset = 0;
    for (unsigned size = 64; size >= 16; size /= 2) {
        unsigned n = size / 2;
        for (unsigned y = 0; y < n; ++y) {
            unsigned right = 2 * y + 2 < size ? 2 * y + 2 : size - 2;
            for (unsigned x = 0; x < size; x += 8) {
                __m256i even = load8(input + 2 * y * size + x);
                __m256i odd = load8(input + (2 * y + 1) * size + x);
                __m256i next = load8(input + right * size + x);
                __m256i value = _mm256_sub_epi32(_mm256_sub_epi32(_mm256_add_epi32(odd, odd), even), next);
                store8(vertical + (n + y) * size + x, _mm256_srai_epi32(_mm256_add_epi32(value, _mm256_set1_epi32(2)), 2));
            }
        }
        for (unsigned y = 0; y < n; ++y) {
            unsigned previous = y ? y - 1 : 0;
            for (unsigned x = 0; x < size; x += 8) {
                __m256i a = load8(vertical + (n + previous) * size + x);
                __m256i b = load8(vertical + (n + y) * size + x);
                __m256i even = load8(input + 2 * y * size + x);
                __m256i sum = _mm256_add_epi32(_mm256_add_epi32(a, b), _mm256_set1_epi32(1));
                store8(vertical + y * size + x, _mm256_add_epi32(even, _mm256_srai_epi32(sum, 1)));
            }
        }
        for (unsigned y = 0; y < size; ++y) {
            dwt_line8(vertical + y * size, low, high, size);
            unsigned low_band = y < n ? 3 : 1, high_band = y < n ? 0 : 2;
            memcpy(bands + low_band * n * n + (y % n) * n, low, n * sizeof(int32_t));
            memcpy(bands + high_band * n * n + (y % n) * n, high, n * sizeof(int32_t));
        }
        size_t count = 3 * n * n;
        memcpy(coeffs + offset, bands, count * sizeof(int32_t));
        offset += count;
        input = bands + count;
    }
    memcpy(coeffs + offset, input, 64 * sizeof(int32_t));
    unsigned quant_bits = quantization - 1;
    __m128i quant_shift = _mm_cvtsi32_si128((int)quant_bits);
    __m256i round = _mm256_set1_epi32((int)(1u << (quant_bits - 1)));
    for (size_t i = 0; i < 4096; i += 8) {
        store8(coeffs + i, _mm256_sra_epi32(_mm256_add_epi32(load8(coeffs + i), round), quant_shift));
    }
    for (size_t i = 4088; i >= 4040; i -= 8) {
        store8(coeffs + i, _mm256_sub_epi32(load8(coeffs + i), load8(coeffs + i - 1)));
    }
    for (size_t i = 4039; i > 4032; --i) coeffs[i] -= coeffs[i - 1];
    _mm256_zeroupper();
}

static inline ANYKVM_RFX_TARGET_AVX2 __m256i color8(__m256i a, __m256i b, int16_t wb, int16_t wg, int16_t wr) {
    __m128i four_weights = _mm_setr_epi16(wb, wg, wr, 0, wb, wg, wr, 0);
    __m256i weights = _mm256_broadcastsi128_si256(four_weights);
    a = _mm256_madd_epi16(a, weights); b = _mm256_madd_epi16(b, weights);
    a = _mm256_add_epi32(a, _mm256_shuffle_epi32(a, 0xb1));
    b = _mm256_add_epi32(b, _mm256_shuffle_epi32(b, 0xb1));
    return _mm256_unpacklo_epi64(_mm256_shuffle_epi32(a, 0x88), _mm256_shuffle_epi32(b, 0x88));
}

ANYKVM_RFX_TARGET_AVX2 void bk_rfx_ict_avx2(int32_t planes[3][4096], const uint8_t *pixels,
                                         unsigned width, unsigned height, unsigned x, unsigned y) {
    unsigned columns = width - x < 64 ? width - x : 64;
    for (unsigned yy = 0; yy < 64; ++yy) {
        unsigned row = y + yy < height ? y + yy : height - 1;
        const uint8_t *source = pixels + ((size_t)row * width + x) * 4;
        unsigned offset = yy * 64, xx = 0;
        for (; xx + 8 <= columns; xx += 8) {
            __m256i bgra = _mm256_loadu_si256((const __m256i *)(source + 4 * xx));
            __m256i a = _mm256_unpacklo_epi8(bgra, _mm256_setzero_si256());
            __m256i b = _mm256_unpackhi_epi8(bgra, _mm256_setzero_si256());
            __m256i yv = color8(a, b, 3735, 19235, 9798);
            __m256i uv = color8(a, b, 16403, -10868, -5535);
            __m256i vv = color8(a, b, -2663, -13714, 16377);
            store8(planes[0] + offset + xx, _mm256_sub_epi32(_mm256_srai_epi32(yv, 10), _mm256_set1_epi32(4096)));
            store8(planes[1] + offset + xx, _mm256_srai_epi32(uv, 10));
            store8(planes[2] + offset + xx, _mm256_srai_epi32(vv, 10));
        }
        for (; xx < columns; ++xx) {
            int32_t b = source[4 * xx], g = source[4 * xx + 1], r = source[4 * xx + 2];
            planes[0][offset + xx] = ((9798 * r + 19235 * g + 3735 * b) >> 10) - 4096;
            planes[1][offset + xx] = (-5535 * r - 10868 * g + 16403 * b) >> 10;
            planes[2][offset + xx] = (16377 * r - 13714 * g - 2663 * b) >> 10;
        }
        for (; xx < 64; ++xx) {
            for (unsigned c = 0; c < 3; ++c) planes[c][offset + xx] = planes[c][offset + columns - 1];
        }
    }
    _mm256_zeroupper();
}
static inline ANYKVM_RFX_TARGET_AVX2 __m256i nv12_rgb8(__m256i value) {
    value = _mm256_srai_epi32(_mm256_add_epi32(value, _mm256_set1_epi32(128)), 8);
    return _mm256_min_epi32(_mm256_max_epi32(value, _mm256_setzero_si256()), _mm256_set1_epi32(255));
}

ANYKVM_RFX_TARGET_AVX2 void bk_rfx_ict_nv12_avx2(int32_t planes[3][4096], const uint8_t *pixels,
                                         unsigned width, unsigned height, unsigned x, unsigned y) {
    unsigned columns = width - x < 64 ? width - x : 64;
    size_t uv_stride = 2 * ((width + 1) / 2);
    for (unsigned yy = 0; yy < 64; ++yy) {
        unsigned row = y + yy < height ? y + yy : height - 1;
        const uint8_t *luma = pixels + (size_t)row * width + x;
        const uint8_t *chroma = pixels + (size_t)width * height + (row / 2) * uv_stride + x;
        unsigned offset = yy * 64, xx = 0;
        for (; xx + 8 <= columns; xx += 8) {
            __m128i uv = _mm_loadl_epi64((const __m128i *)(chroma + xx));
            __m256i yv = _mm256_sub_epi32(_mm256_cvtepu8_epi32(_mm_loadl_epi64((const __m128i *)(luma + xx))), _mm256_set1_epi32(16));
            yv = _mm256_mullo_epi32(yv, _mm256_set1_epi32(298));
            __m256i u = _mm256_sub_epi32(_mm256_cvtepu8_epi32(_mm_shuffle_epi8(uv,
                _mm_setr_epi8(0,0,2,2,4,4,6,6,-128,-128,-128,-128,-128,-128,-128,-128))), _mm256_set1_epi32(128));
            __m256i v = _mm256_sub_epi32(_mm256_cvtepu8_epi32(_mm_shuffle_epi8(uv,
                _mm_setr_epi8(1,1,3,3,5,5,7,7,-128,-128,-128,-128,-128,-128,-128,-128))), _mm256_set1_epi32(128));
            __m256i b = nv12_rgb8(_mm256_add_epi32(yv, _mm256_mullo_epi32(u, _mm256_set1_epi32(541))));
            __m256i g = nv12_rgb8(_mm256_sub_epi32(_mm256_sub_epi32(yv,
                _mm256_mullo_epi32(u, _mm256_set1_epi32(55))), _mm256_mullo_epi32(v, _mm256_set1_epi32(136))));
            __m256i r = nv12_rgb8(_mm256_add_epi32(yv, _mm256_mullo_epi32(v, _mm256_set1_epi32(459))));
            __m256i yp = _mm256_add_epi32(_mm256_add_epi32(_mm256_mullo_epi32(r, _mm256_set1_epi32(9798)),
                _mm256_mullo_epi32(g, _mm256_set1_epi32(19235))), _mm256_mullo_epi32(b, _mm256_set1_epi32(3735)));
            __m256i up = _mm256_add_epi32(_mm256_add_epi32(_mm256_mullo_epi32(r, _mm256_set1_epi32(-5535)),
                _mm256_mullo_epi32(g, _mm256_set1_epi32(-10868))), _mm256_mullo_epi32(b, _mm256_set1_epi32(16403)));
            __m256i vp = _mm256_add_epi32(_mm256_add_epi32(_mm256_mullo_epi32(r, _mm256_set1_epi32(16377)),
                _mm256_mullo_epi32(g, _mm256_set1_epi32(-13714))), _mm256_mullo_epi32(b, _mm256_set1_epi32(-2663)));
            store8(planes[0] + offset + xx, _mm256_sub_epi32(_mm256_srai_epi32(yp, 10), _mm256_set1_epi32(4096)));
            store8(planes[1] + offset + xx, _mm256_srai_epi32(up, 10));
            store8(planes[2] + offset + xx, _mm256_srai_epi32(vp, 10));
        }
        for (; xx < columns; ++xx) {
            int32_t yv = 298 * ((int32_t)luma[xx] - 16);
            int32_t u = (int32_t)chroma[xx / 2 * 2] - 128, v = (int32_t)chroma[xx / 2 * 2 + 1] - 128;
            int32_t b = (yv + 541*u + 128) >> 8, g = (yv - 55*u - 136*v + 128) >> 8, r = (yv + 459*v + 128) >> 8;
            b = b < 0 ? 0 : b > 255 ? 255 : b;
            g = g < 0 ? 0 : g > 255 ? 255 : g;
            r = r < 0 ? 0 : r > 255 ? 255 : r;
            planes[0][offset + xx] = ((9798*r + 19235*g + 3735*b) >> 10) - 4096;
            planes[1][offset + xx] = (-5535*r - 10868*g + 16403*b) >> 10;
            planes[2][offset + xx] = (16377*r - 13714*g - 2663*b) >> 10;
        }
        for (; xx < 64; ++xx) {
            for (unsigned c = 0; c < 3; ++c) planes[c][offset + xx] = planes[c][offset + columns - 1];
        }
    }
    _mm256_zeroupper();
}
#endif
