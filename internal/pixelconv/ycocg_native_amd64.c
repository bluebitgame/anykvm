//go:build cgo && amd64 && (darwin || linux || windows)

/* Independent SIMD kernels for limited-range BT.709 and RDP YCoCg420.
 * No frame buffers or Go pointers are retained. All loads are unaligned and
 * bounded by the active row width; partial 2x2 edge cells use scalar code. */
#include "ycocg_native.h"
#include <emmintrin.h>
#include <immintrin.h>
#include <string.h>

#define TARGET_AVX2 __attribute__((target("avx2")))
typedef struct {
    __m128i b, g, r;
} rgb4;
typedef struct {
    __m256i b, g, r;
} rgb8;

static inline __m128i expand4(__m128i a) {
    const __m128i z = _mm_setzero_si128();
    return _mm_unpacklo_epi16(_mm_unpacklo_epi8(a, z), z);
}
static inline __m128i mul4(__m128i a, int k) {
    /* Each input fits in a signed word. PMADDWD supplies exact signed dword
     * products without requiring SSE4.1 PMULLD. High-word coefficients are 0. */
    return _mm_madd_epi16(a, _mm_set1_epi32((unsigned short)k));
}
static inline __m128i clamp4(__m128i a) {
    const __m128i z = _mm_setzero_si128(), hi = _mm_set1_epi32(255);
    a = _mm_andnot_si128(_mm_cmpgt_epi32(z, a), a);
    const __m128i over = _mm_cmpgt_epi32(a, hi);
    return _mm_or_si128(_mm_and_si128(over, hi), _mm_andnot_si128(over, a));
}
static inline rgb4 bgra4(const uint8_t *p) {
    const __m128i a = _mm_loadu_si128((const __m128i *)p), mask = _mm_set1_epi32(255);
    return (rgb4){_mm_and_si128(a, mask), _mm_and_si128(_mm_srli_epi32(a, 8), mask),
                  _mm_and_si128(_mm_srli_epi32(a, 16), mask)};
}
static inline rgb4 nv4(const uint8_t *p, __m128i uv) {
    unsigned value;
    memcpy(&value, p, 4);
    const __m128i mask = _mm_set1_epi32(255), bias = _mm_set1_epi32(128);
    const __m128i u = _mm_sub_epi32(_mm_and_si128(uv, mask), bias);
    const __m128i v = _mm_sub_epi32(_mm_srli_epi32(uv, 8), bias);
    const __m128i y =
        mul4(_mm_sub_epi32(expand4(_mm_cvtsi32_si128(value)), _mm_set1_epi32(16)), 298);
    return (rgb4){
        clamp4(_mm_srai_epi32(_mm_add_epi32(_mm_add_epi32(y, mul4(u, 541)), bias), 8)),
        clamp4(_mm_srai_epi32(
            _mm_add_epi32(_mm_add_epi32(_mm_add_epi32(y, mul4(u, -55)), mul4(v, -136)), bias), 8)),
        clamp4(_mm_srai_epi32(_mm_add_epi32(_mm_add_epi32(y, mul4(v, 459)), bias), 8))};
}
static inline __m128i uv4(const uint8_t *p) {
    unsigned value;
    memcpy(&value, p, 4);
    __m128i a = _mm_cvtsi32_si128(value);
    return _mm_unpacklo_epi16(_mm_unpacklo_epi16(a, a), _mm_setzero_si128());
}
static inline __m128i co4(rgb4 a) {
    return _mm_sub_epi32(a.b, a.r);
}
static inline __m128i cg4(rgb4 a) {
    return _mm_sub_epi32(_mm_add_epi32(a.g, a.g), _mm_add_epi32(a.r, a.b));
}
static inline void put_y4(uint8_t *p, rgb4 a) {
    __m128i y = _mm_srli_epi32(_mm_add_epi32(_mm_add_epi32(a.r, a.b), _mm_add_epi32(a.g, a.g)), 2);
    y = _mm_packus_epi16(_mm_packs_epi32(y, y), _mm_setzero_si128());
    unsigned value = (unsigned)_mm_cvtsi128_si32(y);
    memcpy(p, &value, 4);
}
static inline void put_c2(uint8_t *p, __m128i a, __m128i b, int shift) {
    __m128i c = _mm_add_epi32(a, b);
    c = _mm_add_epi32(c, _mm_shuffle_epi32(c, _MM_SHUFFLE(2, 3, 0, 1)));
    c = _mm_sra_epi32(c, _mm_cvtsi32_si128(shift));
    c = _mm_shuffle_epi32(c, _MM_SHUFFLE(2, 0, 2, 0));
    c = _mm_packs_epi16(_mm_packs_epi32(c, c), _mm_setzero_si128());
    unsigned short value = (unsigned short)_mm_cvtsi128_si32(c);
    memcpy(p, &value, 2);
}
static inline void cell4(uint8_t *y0, uint8_t *y1, uint8_t *co, uint8_t *cg, rgb4 a, rgb4 b) {
    put_y4(y0, a);
    put_y4(y1, b);
    put_c2(co, co4(a), co4(b), 3);
    put_c2(cg, cg4(a), cg4(b), 4);
}
static inline int clip(int v) {
    return v < 0 ? 0 : (v > 255 ? 255 : v);
}
static void cell_edge(uint8_t *dst, const uint8_t *first, const uint8_t *uv, int fs, int us, int w,
                      int h, int x, int y, int nv12) {
    int sum_co = 0, sum_cg = 0, cw = (w + 1) / 2, ch = (h + 1) / 2;
    for (int dy = 0; dy < 2; ++dy)
        for (int dx = 0; dx < 2; ++dx) {
            int xx = x + dx < w ? x + dx : w - 1, yy = y + dy < h ? y + dy : h - 1;
            int b, g, r;
            if (nv12) {
                const uint8_t *c = uv + (yy / 2) * us + (xx / 2) * 2;
                int yp = 298 * ((int)first[yy * fs + xx] - 16), u = (int)c[0] - 128,
                    v = (int)c[1] - 128;
                b = clip((yp + 541 * u + 128) >> 8);
                g = clip((yp - 55 * u - 136 * v + 128) >> 8);
                r = clip((yp + 459 * v + 128) >> 8);
            } else {
                const uint8_t *p = first + yy * fs + xx * 4;
                b = p[0];
                g = p[1];
                r = p[2];
            }
            dst[yy * w + xx] = (uint8_t)((r + 2 * g + b) >> 2);
            sum_co += b - r;
            sum_cg += 2 * g - r - b;
        }
    int i = (y / 2) * cw + x / 2;
    dst[w * h + i] = (uint8_t)(sum_co >> 3);
    dst[w * h + cw * ch + i] = (uint8_t)(sum_cg >> 4);
}
static void convert_sse2(int nv12, uint8_t *dst, const uint8_t *first, const uint8_t *uv, int fs,
                         int us, int w, int h) {
    const int cw = (w + 1) / 2, ch = (h + 1) / 2;
    for (int y = 0; y < h; y += 2) {
        const int yb = y + 1 < h ? y + 1 : y;
        int x = 0;
        for (; x + 4 <= w; x += 4) {
            rgb4 a, b;
            if (nv12) {
                __m128i c = uv4(uv + (y / 2) * us + x);
                a = nv4(first + y * fs + x, c);
                b = nv4(first + yb * fs + x, c);
            } else {
                a = bgra4(first + y * fs + x * 4);
                b = bgra4(first + yb * fs + x * 4);
            }
            const int i = (y / 2) * cw + x / 2;
            cell4(dst + y * w + x, dst + yb * w + x, dst + w * h + i, dst + w * h + cw * ch + i, a,
                  b);
        }
        for (; x < w; x += 2)
            cell_edge(dst, first, uv, fs, us, w, h, x, y, nv12);
    }
}
TARGET_AVX2 static inline rgb8 bgra8(const uint8_t *p) {
    const __m256i a = _mm256_loadu_si256((const __m256i *)p), mask = _mm256_set1_epi32(255);
    return (rgb8){_mm256_and_si256(a, mask), _mm256_and_si256(_mm256_srli_epi32(a, 8), mask),
                  _mm256_and_si256(_mm256_srli_epi32(a, 16), mask)};
}
TARGET_AVX2 static inline __m256i clamp8(__m256i a) {
    return _mm256_min_epi32(_mm256_max_epi32(a, _mm256_setzero_si256()), _mm256_set1_epi32(255));
}
TARGET_AVX2 static inline __m256i uv8(const uint8_t *p) {
    const __m128i a = _mm_loadl_epi64((const __m128i *)p);
    return _mm256_cvtepu16_epi32(_mm_unpacklo_epi16(a, a));
}
TARGET_AVX2 static inline rgb8 nv8(const uint8_t *p, __m256i uv) {
    const __m256i mask = _mm256_set1_epi32(255), bias = _mm256_set1_epi32(128);
    const __m256i u = _mm256_sub_epi32(_mm256_and_si256(uv, mask), bias),
                  v = _mm256_sub_epi32(_mm256_srli_epi32(uv, 8), bias);
    const __m256i y = _mm256_mullo_epi32(
        _mm256_sub_epi32(_mm256_cvtepu8_epi32(_mm_loadl_epi64((const __m128i *)p)),
                         _mm256_set1_epi32(16)),
        _mm256_set1_epi32(298));
    return (rgb8){
        clamp8(_mm256_srai_epi32(
            _mm256_add_epi32(_mm256_add_epi32(y, _mm256_mullo_epi32(u, _mm256_set1_epi32(541))),
                             bias),
            8)),
        clamp8(_mm256_srai_epi32(
            _mm256_add_epi32(
                _mm256_sub_epi32(_mm256_sub_epi32(y, _mm256_mullo_epi32(u, _mm256_set1_epi32(55))),
                                 _mm256_mullo_epi32(v, _mm256_set1_epi32(136))),
                bias),
            8)),
        clamp8(_mm256_srai_epi32(
            _mm256_add_epi32(_mm256_add_epi32(y, _mm256_mullo_epi32(v, _mm256_set1_epi32(459))),
                             bias),
            8))};
}
TARGET_AVX2 static inline __m256i co8(rgb8 a) {
    return _mm256_sub_epi32(a.b, a.r);
}
TARGET_AVX2 static inline __m256i cg8(rgb8 a) {
    return _mm256_sub_epi32(_mm256_add_epi32(a.g, a.g), _mm256_add_epi32(a.r, a.b));
}
TARGET_AVX2 static inline void put_y8(uint8_t *p, rgb8 a) {
    __m256i y = _mm256_srli_epi32(
        _mm256_add_epi32(_mm256_add_epi32(a.r, a.b), _mm256_add_epi32(a.g, a.g)), 2);
    __m128i lo = _mm256_castsi256_si128(y), hi = _mm256_extracti128_si256(y, 1);
    _mm_storel_epi64((__m128i *)p, _mm_packus_epi16(_mm_packs_epi32(lo, hi), _mm_setzero_si128()));
}
TARGET_AVX2 static inline void put_c4(uint8_t *p, __m256i a, __m256i b, int shift) {
    __m256i c = _mm256_add_epi32(a, b);
    c = _mm256_hadd_epi32(c, c);
    __m128i pairs = _mm_unpacklo_epi64(_mm256_castsi256_si128(c), _mm256_extracti128_si256(c, 1));
    pairs = _mm_sra_epi32(pairs, _mm_cvtsi32_si128(shift));
    pairs = _mm_packs_epi16(_mm_packs_epi32(pairs, pairs), _mm_setzero_si128());
    unsigned value = (unsigned)_mm_cvtsi128_si32(pairs);
    memcpy(p, &value, 4);
}
TARGET_AVX2 static void convert_avx2(int nv12, uint8_t *dst, const uint8_t *first,
                                     const uint8_t *uv, int fs, int us, int w, int h) {
    const int cw = (w + 1) / 2, ch = (h + 1) / 2;
    for (int y = 0; y < h; y += 2) {
        const int yb = y + 1 < h ? y + 1 : y;
        int x = 0;
        for (; x + 8 <= w; x += 8) {
            rgb8 a, b;
            if (nv12) {
                __m256i c = uv8(uv + (y / 2) * us + x);
                a = nv8(first + y * fs + x, c);
                b = nv8(first + yb * fs + x, c);
            } else {
                a = bgra8(first + y * fs + x * 4);
                b = bgra8(first + yb * fs + x * 4);
            }
            const int i = (y / 2) * cw + x / 2;
            put_y8(dst + y * w + x, a);
            put_y8(dst + yb * w + x, b);
            put_c4(dst + w * h + i, co8(a), co8(b), 3);
            put_c4(dst + w * h + cw * ch + i, cg8(a), cg8(b), 4);
        }
        for (; x + 4 <= w; x += 4) {
            rgb4 a, b;
            if (nv12) {
                __m128i c = uv4(uv + (y / 2) * us + x);
                a = nv4(first + y * fs + x, c);
                b = nv4(first + yb * fs + x, c);
            } else {
                a = bgra4(first + y * fs + x * 4);
                b = bgra4(first + yb * fs + x * 4);
            }
            const int i = (y / 2) * cw + x / 2;
            cell4(dst + y * w + x, dst + yb * w + x, dst + w * h + i, dst + w * h + cw * ch + i, a,
                  b);
        }
        for (; x < w; x += 2)
            cell_edge(dst, first, uv, fs, us, w, h, x, y, nv12);
    }
}
void bk_ycocg420(int avx2, int nv12, uint8_t *dst, const uint8_t *first, const uint8_t *uv, int fs,
                 int us, int w, int h) {
    if (avx2)
        convert_avx2(nv12, dst, first, uv, fs, us, w, h);
    else
        convert_sse2(nv12, dst, first, uv, fs, us, w, h);
}
static inline void delta16(uint8_t *dst, const uint8_t *row, const uint8_t *prev) {
    const __m128i d =
        _mm_sub_epi8(_mm_loadu_si128((const __m128i *)row), _mm_loadu_si128((const __m128i *)prev));
    _mm_storeu_si128((__m128i *)dst,
                     _mm_xor_si128(_mm_add_epi8(d, d), _mm_cmpgt_epi8(_mm_setzero_si128(), d)));
}
static void delta_tail(uint8_t *dst, const uint8_t *row, const uint8_t *prev, int start, int size) {
    for (int i = start; i < size; ++i) {
        int d = (int8_t)(row[i] - prev[i]);
        dst[i] = (uint8_t)(d < 0 ? -2 * d - 1 : 2 * d);
    }
}
TARGET_AVX2 static void delta_avx2(uint8_t *dst, const uint8_t *row, const uint8_t *prev,
                                   int size) {
    int i = 0;
    for (; i + 32 <= size; i += 32) {
        __m256i d = _mm256_sub_epi8(_mm256_loadu_si256((const __m256i *)(row + i)),
                                    _mm256_loadu_si256((const __m256i *)(prev + i)));
        _mm256_storeu_si256(
            (__m256i *)(dst + i),
            _mm256_xor_si256(_mm256_add_epi8(d, d), _mm256_cmpgt_epi8(_mm256_setzero_si256(), d)));
    }
    for (; i + 16 <= size; i += 16)
        delta16(dst + i, row + i, prev + i);
    delta_tail(dst, row, prev, i, size);
}
void bk_planar_delta(int avx2, uint8_t *dst, const uint8_t *row, const uint8_t *prev, int size) {
    if (avx2) {
        delta_avx2(dst, row, prev, size);
        return;
    }
    int i = 0;
    for (; i + 16 <= size; i += 16)
        delta16(dst + i, row + i, prev + i);
    delta_tail(dst, row, prev, i, size);
}
