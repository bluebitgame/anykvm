//go:build cgo && (darwin || linux || windows)

// SIMPLE and layered Progressive encoder. The algorithm follows
// MS-RDPEGFX 2.2.4.2 / 3.2.8.1 and MS-RDPRFX 3.1.8.1, matching progressive.go.
#include "progressive_native.h"
#include "progressive_simd.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#if defined(__SSE2__) && (defined(__x86_64__) || defined(_M_X64))
#include <emmintrin.h>
#define RFX_SSE2 1
#elif defined(__aarch64__) || defined(_M_ARM64)
#include <arm_neon.h>
#define RFX_NEON 1
#endif

#if defined(ANYKVM_RFX_AVX2)
#include <cpuid.h>

// Baseline-only feature probe. Never execute XGETBV before OSXSAVE is set,
// and never enter an AVX2 kernel before the OS enables both XMM and YMM state.
static int avx2_available(void) {
    unsigned eax, ebx, ecx, edx;
    if (!__get_cpuid(1, &eax, &ebx, &ecx, &edx)) return 0;
    const unsigned required = (1u << 26) | (1u << 27) | (1u << 28);
    if ((ecx & required) != required) return 0;
    uint32_t xcr0, upper;
    __asm__ volatile("xgetbv" : "=a"(xcr0), "=d"(upper) : "c"(0));
    (void)upper;
    if ((xcr0 & 6) != 6) return 0;
    if (!__get_cpuid_count(7, 0, &eax, &ebx, &ecx, &edx)) return 0;
    return (ebx & (1u << 5)) != 0;
}
#endif

// Four signed fixed-point values. Both architectures provide arithmetic shifts
// with the same negative-value rounding as the scalar lifting equations.
#if defined(RFX_SSE2)
typedef __m128i rfx_vec;
static inline rfx_vec vload(const int32_t *p) { return _mm_loadu_si128((const __m128i *)p); }
static inline void vstore(int32_t *p, rfx_vec v) { _mm_storeu_si128((__m128i *)p, v); }
static inline rfx_vec vconstant(int32_t v) { return _mm_set1_epi32(v); }
static inline rfx_vec vadd(rfx_vec a, rfx_vec b) { return _mm_add_epi32(a, b); }
static inline rfx_vec vsub(rfx_vec a, rfx_vec b) { return _mm_sub_epi32(a, b); }
#define vshift(v, n) _mm_srai_epi32((v), (n))
static inline rfx_vec vquant_shift(rfx_vec v, unsigned bits) { return _mm_sra_epi32(v, _mm_cvtsi32_si128((int)bits)); }
static inline void veven_odd(const int32_t *p, rfx_vec *even, rfx_vec *odd) {
    rfx_vec a = vload(p), b = vload(p + 4);
    *even = _mm_unpacklo_epi64(_mm_shuffle_epi32(a, 0x88), _mm_shuffle_epi32(b, 0x88));
    *odd = _mm_unpacklo_epi64(_mm_shuffle_epi32(a, 0xdd), _mm_shuffle_epi32(b, 0xdd));
}
static inline rfx_vec vnext(rfx_vec v, int32_t next) {
    return _mm_or_si128(_mm_srli_si128(v, 4), _mm_slli_si128(_mm_cvtsi32_si128(next), 12));
}
static inline rfx_vec vprevious(rfx_vec v, int32_t previous) {
    return _mm_or_si128(_mm_slli_si128(v, 4), _mm_cvtsi32_si128(previous));
}
#elif defined(RFX_NEON)
typedef int32x4_t rfx_vec;
static inline rfx_vec vload(const int32_t *p) { return vld1q_s32(p); }
static inline void vstore(int32_t *p, rfx_vec v) { vst1q_s32(p, v); }
static inline rfx_vec vconstant(int32_t v) { return vdupq_n_s32(v); }
static inline rfx_vec vadd(rfx_vec a, rfx_vec b) { return vaddq_s32(a, b); }
static inline rfx_vec vsub(rfx_vec a, rfx_vec b) { return vsubq_s32(a, b); }
#define vshift(v, n) vshrq_n_s32((v), (n))
static inline rfx_vec vquant_shift(rfx_vec v, unsigned bits) { return vshlq_s32(v, vdupq_n_s32(-(int)bits)); }
static inline void veven_odd(const int32_t *p, rfx_vec *even, rfx_vec *odd) {
    int32x4x2_t values = vld2q_s32(p);
    *even = values.val[0]; *odd = values.val[1];
}
static inline rfx_vec vnext(rfx_vec v, int32_t next) { return vextq_s32(v, vdupq_n_s32(next), 1); }
static inline rfx_vec vprevious(rfx_vec v, int32_t previous) { return vextq_s32(vdupq_n_s32(previous), v, 3); }
#endif

#define MAX_OUTPUT (256u * 1024u)
typedef struct {
    int32_t planes[3][4096];
    int32_t vertical[4096], bands[4096], coeffs[4096];
    uint8_t *output;
    size_t length, capacity;
    uint8_t pending;
    unsigned used, quantization;
    int failed, simd;
} rfx_encoder;

// Explicit floor division makes the fixed-point math portable for negatives.
static inline int32_t shift(int32_t value, unsigned bits) {
#if ((-1 >> 1) == -1)
    return value >> bits;
#else
    if (value >= 0) return (int32_t)((uint32_t)value >> bits);
    return -1 - (int32_t)((uint32_t)(-1 - value) >> bits);
#endif
}

static inline void byte(rfx_encoder *e, uint8_t value) {
    if (e->length == e->capacity) {
        if (e->capacity == MAX_OUTPUT) { e->failed = 1; return; }
        size_t capacity = e->capacity * 2;
        if (capacity > MAX_OUTPUT) capacity = MAX_OUTPUT;
        uint8_t *output = realloc(e->output, capacity);
        if (!output) { e->failed = 1; return; }
        e->output = output;
        e->capacity = capacity;
    }
    e->output[e->length++] = value;
}
static inline void word(rfx_encoder *e, uint16_t value) {
    byte(e, (uint8_t)value); byte(e, (uint8_t)(value >> 8));
}
static inline void dword(rfx_encoder *e, uint32_t value) {
    word(e, (uint16_t)value); word(e, (uint16_t)(value >> 16));
}
static void patch_word(rfx_encoder *e, size_t offset, uint16_t value) {
    e->output[offset] = (uint8_t)value;
    e->output[offset + 1] = (uint8_t)(value >> 8);
}
static void patch_dword(rfx_encoder *e, size_t offset, uint32_t value) {
    patch_word(e, offset, (uint16_t)value);
    patch_word(e, offset + 2, (uint16_t)(value >> 16));
}
static inline void block(rfx_encoder *e, uint16_t kind, uint32_t length) {
    word(e, kind); dword(e, length);
}

// MSB-first writes consume whole bytes instead of iterating once per bit.
static inline void bits(rfx_encoder *e, uint32_t value, unsigned count) {
    if (e->used && count) {
        unsigned take = 8 - e->used;
        if (take > count) take = count;
        count -= take;
        e->pending |= (uint8_t)(((value >> count) & ((1u << take) - 1)) << (8 - e->used - take));
        e->used += take;
        if (e->used == 8) { byte(e, e->pending); e->pending = 0; e->used = 0; }
    }
    while (count >= 8) {
        count -= 8;
        byte(e, (uint8_t)(value >> count));
    }
    if (count) {
        e->pending = (uint8_t)(value << (8 - count));
        e->used = count;
    }
}
static inline void ones(rfx_encoder *e, unsigned count) {
    while (count >= 32) { bits(e, UINT32_MAX, 32); count -= 32; }
    if (count) bits(e, UINT32_MAX, count);
}
static inline unsigned clamp80(unsigned value) { return value < 80 ? value : 80; }
static inline void rice(rfx_encoder *e, uint32_t value, unsigned *krp) {
    unsigned kr = *krp >> 3;
    unsigned quotient = value >> kr;
    unsigned count = quotient + 1 + kr;
    if (count <= 32) {
        // Pack the unary prefix, zero delimiter and Rice remainder into one
        // MSB-first write. Most coefficients take this path; adaptation and
        // wire bytes are identical to the separate writes below.
        uint32_t code = (UINT32_MAX << (kr + 1)) | (value & ((1u << kr) - 1));
        bits(e, code, count);
    } else {
        ones(e, quotient);
        bits(e, 0, 1);
        bits(e, value, kr);
    }
    if (quotient == 0) *krp = *krp > 2 ? *krp - 2 : 0;
    else if (quotient > 1) *krp = clamp80(*krp + quotient);
}
static void rlgr(rfx_encoder *e) {
    unsigned kp = 8, krp = 8;
    e->pending = 0; e->used = 0;
    for (size_t i = 0; i < 4096 && !e->failed; ++i) {
        unsigned k = kp >> 3;
        if (k) {
            unsigned run = 0;
            while (i < 4096 && e->coeffs[i] == 0) { ++run; ++i; }
            while (run >= (1u << k)) {
                bits(e, 0, 1);
                run -= 1u << k;
                kp = clamp80(kp + 4);
                k = kp >> 3;
            }
            bits(e, 1, 1);
            bits(e, run, k);
            if (i == 4096) break;
            int32_t value = e->coeffs[i];
            bits(e, value < 0, 1);
            uint32_t magnitude = (uint32_t)(value < 0 ? -value : value);
            rice(e, magnitude - 1, &krp);
            kp = kp > 6 ? kp - 6 : 0;
        } else {
            int32_t value = e->coeffs[i];
            uint32_t code = (uint32_t)(value < 0 ? -2 * value - 1 : 2 * value);
            rice(e, code, &krp);
            if (code == 0) kp = clamp80(kp + 3);
            else kp = kp > 3 ? kp - 3 : 0;
        }
    }
    if (e->used) byte(e, e->pending);
    e->pending = 0; e->used = 0;
}

static void dwt_line(const int32_t *restrict input, int32_t *restrict low, int32_t *restrict high, unsigned size, int simd) {
    unsigned n = size / 2;
#if defined(RFX_SSE2) || defined(RFX_NEON)
    if (simd) {
        for (unsigned i = 0; i < n; i += 4) {
            rfx_vec even, odd;
            veven_odd(input + 2 * i, &even, &odd);
            unsigned right = 2 * i + 8 < size ? 2 * i + 8 : size - 2;
            rfx_vec value = vsub(vsub(vadd(odd, odd), even), vnext(even, input[right]));
            vstore(high + i, vshift(vadd(value, vconstant(2)), 2));
        }
        for (unsigned i = 0; i < n; i += 4) {
            rfx_vec even, odd;
            veven_odd(input + 2 * i, &even, &odd);
            rfx_vec current = vload(high + i);
            rfx_vec sum = vadd(vprevious(current, high[i ? i - 1 : 0]), current);
            vstore(low + i, vadd(even, vshift(vadd(sum, vconstant(1)), 1)));
        }
        return;
    }
#else
    (void)simd;
#endif
    for (unsigned i = 0; i < n; ++i) {
        unsigned right = 2 * i + 2;
        if (right >= size) right = size - 2;
        high[i] = shift(2 * input[2 * i + 1] - input[2 * i] - input[right] + 2, 2);
    }
    for (unsigned i = 0; i < n; ++i) {
        low[i] = input[2 * i] + shift(high[i ? i - 1 : 0] + high[i] + 1, 1);
    }
}
#define ANYKVM_REDUCE_TARGET
#define ANYKVM_REDUCE_LINE dwt_reduce_line
#define ANYKVM_REDUCE_DWT dwt_reduce
#if defined(RFX_SSE2) || defined(RFX_NEON)
#define ANYKVM_REDUCE_LANES 4
#define ANYKVM_REDUCE_VEC rfx_vec
#define ANYKVM_REDUCE_LOAD vload
#define ANYKVM_REDUCE_STORE vstore
#define ANYKVM_REDUCE_CONST vconstant
#define ANYKVM_REDUCE_ADD vadd
#define ANYKVM_REDUCE_SUB vsub
#define ANYKVM_REDUCE_SHIFT vshift
#define ANYKVM_REDUCE_QUANT_SHIFT vquant_shift
#define ANYKVM_REDUCE_EVEN_ODD veven_odd
#define ANYKVM_REDUCE_NEXT vnext
#define ANYKVM_REDUCE_PREVIOUS vprevious
#else
#define ANYKVM_REDUCE_LANES 0
#endif
#include "progressive_reduce.h"

static void dwt(rfx_encoder *e, const int32_t *input) {
#if defined(ANYKVM_RFX_AVX2)
    if (e->simd == ANYKVM_RFX_SIMD_AVX2) {
        bk_rfx_dwt_avx2(input, e->vertical, e->bands, e->coeffs, e->quantization);
        return;
    }
#endif
    int32_t low[32], high[32];
    size_t offset = 0;
    for (unsigned size = 64; size >= 16; size /= 2) {
        unsigned n = size / 2;
        // Apply the column filter across contiguous rows. This preserves the
        // lifting equations while avoiding strided gathers into a temporary
        // column. Each SIMD lane operates on one adjacent column.
        for (unsigned y = 0; y < n; ++y) {
            unsigned right = 2 * y + 2 < size ? 2 * y + 2 : size - 2;
            unsigned x = 0;
#if defined(RFX_SSE2) || defined(RFX_NEON)
            if (e->simd) for (; x < size; x += 4) {
                rfx_vec even = vload(input + 2 * y * size + x);
                rfx_vec odd = vload(input + (2 * y + 1) * size + x);
                rfx_vec next = vload(input + right * size + x);
                rfx_vec value = vsub(vsub(vadd(odd, odd), even), next);
                vstore(e->vertical + (n + y) * size + x, vshift(vadd(value, vconstant(2)), 2));
            }
#endif
            for (; x < size; ++x) {
                e->vertical[(n + y) * size + x] = shift(2 * input[(2 * y + 1) * size + x] -
                    input[2 * y * size + x] - input[right * size + x] + 2, 2);
            }
        }
        for (unsigned y = 0; y < n; ++y) {
            unsigned previous = y ? y - 1 : 0;
            unsigned x = 0;
#if defined(RFX_SSE2) || defined(RFX_NEON)
            if (e->simd) for (; x < size; x += 4) {
                rfx_vec a = vload(e->vertical + (n + previous) * size + x);
                rfx_vec b = vload(e->vertical + (n + y) * size + x);
                rfx_vec even = vload(input + 2 * y * size + x);
                vstore(e->vertical + y * size + x, vadd(even, vshift(vadd(vadd(a, b), vconstant(1)), 1)));
            }
#endif
            for (; x < size; ++x) {
                e->vertical[y * size + x] = input[2 * y * size + x] + shift(
                    e->vertical[(n + previous) * size + x] + e->vertical[(n + y) * size + x] + 1, 1);
            }
        }
        for (unsigned y = 0; y < size; ++y) {
            dwt_line(e->vertical + y * size, low, high, size, e->simd);
            unsigned low_band = y < n ? 3 : 1;
            unsigned high_band = y < n ? 0 : 2;
            memcpy(e->bands + low_band * n * n + (y % n) * n, low, n * sizeof(int32_t));
            memcpy(e->bands + high_band * n * n + (y % n) * n, high, n * sizeof(int32_t));
        }
        size_t count = 3 * n * n;
        memcpy(e->coeffs + offset, e->bands, count * sizeof(int32_t));
        offset += count;
        input = e->bands + count;
    }
    memcpy(e->coeffs + offset, input, 64 * sizeof(int32_t));
    unsigned quant_bits = e->quantization - 1;
    int32_t round = (int32_t)(1u << (quant_bits - 1));
#if defined(RFX_SSE2) || defined(RFX_NEON)
    if (e->simd) {
        for (size_t i = 0; i < 4096; i += 4) {
            vstore(e->coeffs + i, vquant_shift(vadd(vload(e->coeffs + i), vconstant(round)), quant_bits));
        }
        // Work backwards so every subtraction reads the original predecessor.
        for (size_t i = 4092; i >= 4036; i -= 4) {
            vstore(e->coeffs + i, vsub(vload(e->coeffs + i), vload(e->coeffs + i - 1)));
        }
        for (size_t i = 4035; i > 4032; --i) e->coeffs[i] -= e->coeffs[i - 1];
        return;
    }
#endif
    for (size_t i = 0; i < 4096; ++i) e->coeffs[i] = shift(e->coeffs[i] + round, quant_bits);
    for (size_t i = 4095; i > 4032; --i) e->coeffs[i] -= e->coeffs[i - 1];
}

#if defined(RFX_SSE2)
// PMADDWD forms the BG and RA products for two pixels. Sum their pairs,
// then join the two halves to obtain four full fixed-point color values.
static inline rfx_vec color4(rfx_vec a, rfx_vec b, rfx_vec weights) {
    a = _mm_madd_epi16(a, weights); b = _mm_madd_epi16(b, weights);
    a = _mm_add_epi32(a, _mm_shuffle_epi32(a, 0xb1));
    b = _mm_add_epi32(b, _mm_shuffle_epi32(b, 0xb1));
    return _mm_unpacklo_epi64(_mm_shuffle_epi32(a, 0x88), _mm_shuffle_epi32(b, 0x88));
}
#elif defined(RFX_NEON)
static inline rfx_vec color4(int16x4_t b, int16x4_t g, int16x4_t r, int16_t wb, int16_t wg, int16_t wr) {
    return vmlal_n_s16(vmlal_n_s16(vmull_n_s16(b, wb), g, wg), r, wr);
}
#endif
static void ict(rfx_encoder *e, const uint8_t *pixels, unsigned width, unsigned height, unsigned x, unsigned y) {
#if defined(ANYKVM_RFX_AVX2)
    if (e->simd == ANYKVM_RFX_SIMD_AVX2) {
        bk_rfx_ict_avx2(e->planes, pixels, width, height, x, y);
        return;
    }
#endif
    unsigned columns = width - x < 64 ? width - x : 64;
    for (unsigned yy = 0; yy < 64; ++yy) {
        unsigned row = y + yy < height ? y + yy : height - 1;
        const uint8_t *source = pixels + ((size_t)row * width + x) * 4;
        unsigned offset = yy * 64;
        unsigned xx = 0;
#if defined(RFX_SSE2)
        if (e->simd) for (; xx + 4 <= columns; xx += 4) {
            rfx_vec bgra = _mm_loadu_si128((const __m128i *)(source + 4 * xx));
            rfx_vec a = _mm_unpacklo_epi8(bgra, _mm_setzero_si128());
            rfx_vec b = _mm_unpackhi_epi8(bgra, _mm_setzero_si128());
            rfx_vec yv = color4(a, b, _mm_setr_epi16(3735, 19235, 9798, 0, 3735, 19235, 9798, 0));
            rfx_vec uv = color4(a, b, _mm_setr_epi16(16403, -10868, -5535, 0, 16403, -10868, -5535, 0));
            rfx_vec vv = color4(a, b, _mm_setr_epi16(-2663, -13714, 16377, 0, -2663, -13714, 16377, 0));
            vstore(e->planes[0] + offset + xx, vsub(vshift(yv, 10), vconstant(4096)));
            vstore(e->planes[1] + offset + xx, vshift(uv, 10));
            vstore(e->planes[2] + offset + xx, vshift(vv, 10));
        }
#elif defined(RFX_NEON)
        if (e->simd) for (; xx + 8 <= columns; xx += 8) {
            uint8x8x4_t bgra = vld4_u8(source + 4 * xx);
            int16x8_t b = vreinterpretq_s16_u16(vmovl_u8(bgra.val[0]));
            int16x8_t g = vreinterpretq_s16_u16(vmovl_u8(bgra.val[1]));
            int16x8_t r = vreinterpretq_s16_u16(vmovl_u8(bgra.val[2]));
            rfx_vec yl = color4(vget_low_s16(b), vget_low_s16(g), vget_low_s16(r), 3735, 19235, 9798);
            rfx_vec yh = color4(vget_high_s16(b), vget_high_s16(g), vget_high_s16(r), 3735, 19235, 9798);
            rfx_vec ul = color4(vget_low_s16(b), vget_low_s16(g), vget_low_s16(r), 16403, -10868, -5535);
            rfx_vec uh = color4(vget_high_s16(b), vget_high_s16(g), vget_high_s16(r), 16403, -10868, -5535);
            rfx_vec vl = color4(vget_low_s16(b), vget_low_s16(g), vget_low_s16(r), -2663, -13714, 16377);
            rfx_vec vh = color4(vget_high_s16(b), vget_high_s16(g), vget_high_s16(r), -2663, -13714, 16377);
            vstore(e->planes[0] + offset + xx, vsub(vshift(yl, 10), vconstant(4096)));
            vstore(e->planes[0] + offset + xx + 4, vsub(vshift(yh, 10), vconstant(4096)));
            vstore(e->planes[1] + offset + xx, vshift(ul, 10));
            vstore(e->planes[1] + offset + xx + 4, vshift(uh, 10));
            vstore(e->planes[2] + offset + xx, vshift(vl, 10));
            vstore(e->planes[2] + offset + xx + 4, vshift(vh, 10));
        }
#endif
        for (; xx < columns; ++xx) {
            int32_t b = source[4 * xx], g = source[4 * xx + 1], r = source[4 * xx + 2];
            e->planes[0][offset + xx] = shift(9798 * r + 19235 * g + 3735 * b, 10) - 4096;
            e->planes[1][offset + xx] = shift(-5535 * r - 10868 * g + 16403 * b, 10);
            e->planes[2][offset + xx] = shift(16377 * r - 13714 * g - 2663 * b, 10);
        }
        for (unsigned xx = columns; xx < 64; ++xx) {
            for (unsigned c = 0; c < 3; ++c) e->planes[c][offset + xx] = e->planes[c][offset + columns - 1];
        }
    }
}

static inline int32_t clamp255(int32_t v) { return v < 0 ? 0 : v > 255 ? 255 : v; }
#if defined(RFX_SSE2)
static inline rfx_vec multiply16(rfx_vec v, int16_t weight) {
    return _mm_madd_epi16(_mm_unpacklo_epi16(v, _mm_setzero_si128()), _mm_set1_epi32((uint16_t)weight));
}
static inline rfx_vec nv12_rgb4(rfx_vec v) {
    v = vshift(vadd(v, vconstant(128)), 8);
    v = _mm_packus_epi16(_mm_packs_epi32(v, _mm_setzero_si128()), _mm_setzero_si128());
    return _mm_unpacklo_epi8(v, _mm_setzero_si128());
}
#elif defined(RFX_NEON)
static inline int16x8_t nv12_rgb8(int32x4_t low, int32x4_t high) {
    uint16x8_t v = vcombine_u16(vqmovun_s32(vshrq_n_s32(vaddq_s32(low, vdupq_n_s32(128)), 8)),
                              vqmovun_s32(vshrq_n_s32(vaddq_s32(high, vdupq_n_s32(128)), 8)));
    return vreinterpretq_s16_u16(vmovl_u8(vqmovn_u16(v)));
}
#endif

// Consume NV12 directly; retain the old RGB clipping/rounding in registers so
// wire bytes match bitmapBGRA followed by ICT, without an RGB frame buffer.
static void ict_nv12(rfx_encoder *e, const uint8_t *pixels, unsigned width, unsigned height, unsigned x, unsigned y) {
#if defined(ANYKVM_RFX_AVX2)
    if (e->simd == ANYKVM_RFX_SIMD_AVX2) {
        bk_rfx_ict_nv12_avx2(e->planes, pixels, width, height, x, y);
        return;
    }
#endif
    unsigned columns = width - x < 64 ? width - x : 64;
    size_t uv_stride = 2 * ((width + 1) / 2);
    for (unsigned yy = 0; yy < 64; ++yy) {
        unsigned row = y + yy < height ? y + yy : height - 1;
        const uint8_t *luma = pixels + (size_t)row * width + x;
        const uint8_t *chroma = pixels + (size_t)width * height + (row / 2) * uv_stride + x;
        unsigned offset = yy * 64, xx = 0;
#if defined(RFX_SSE2)
        if (e->simd) for (; xx + 4 <= columns; xx += 4) {
            uint32_t yl, uv;
            memcpy(&yl, luma + xx, 4); memcpy(&uv, chroma + xx, 4);
            rfx_vec y16 = _mm_sub_epi16(_mm_unpacklo_epi8(_mm_cvtsi32_si128((int32_t)yl), _mm_setzero_si128()), _mm_set1_epi16(16));
            rfx_vec uv16 = _mm_unpacklo_epi8(_mm_cvtsi32_si128((int32_t)uv), _mm_setzero_si128());
            rfx_vec u16 = _mm_sub_epi16(_mm_shufflelo_epi16(uv16, 0xa0), _mm_set1_epi16(128));
            rfx_vec v16 = _mm_sub_epi16(_mm_shufflelo_epi16(uv16, 0xf5), _mm_set1_epi16(128));
            rfx_vec yv = multiply16(y16, 298);
            rfx_vec b16 = nv12_rgb4(vadd(yv, multiply16(u16, 541)));
            rfx_vec g16 = nv12_rgb4(vsub(vsub(yv, multiply16(u16, 55)), multiply16(v16, 136)));
            rfx_vec r16 = nv12_rgb4(vadd(yv, multiply16(v16, 459)));
            rfx_vec bg = _mm_unpacklo_epi16(b16, g16), rz = _mm_unpacklo_epi16(r16, _mm_setzero_si128());
            rfx_vec a = _mm_unpacklo_epi32(bg, rz), b = _mm_unpackhi_epi32(bg, rz);
            vstore(e->planes[0] + offset + xx, vsub(vshift(color4(a, b, _mm_setr_epi16(3735,19235,9798,0,3735,19235,9798,0)), 10), vconstant(4096)));
            vstore(e->planes[1] + offset + xx, vshift(color4(a, b, _mm_setr_epi16(16403,-10868,-5535,0,16403,-10868,-5535,0)), 10));
            vstore(e->planes[2] + offset + xx, vshift(color4(a, b, _mm_setr_epi16(-2663,-13714,16377,0,-2663,-13714,16377,0)), 10));
        }
#elif defined(RFX_NEON)
        if (e->simd) for (; xx + 8 <= columns; xx += 8) {
            uint8x8_t uv = vld1_u8(chroma + xx);
            int16x8_t y16 = vsubq_s16(vreinterpretq_s16_u16(vmovl_u8(vld1_u8(luma + xx))), vdupq_n_s16(16));
            int16x8_t u16 = vsubq_s16(vreinterpretq_s16_u16(vmovl_u8(vtbl1_u8(uv, vcreate_u8(UINT64_C(0x0606040402020000))))), vdupq_n_s16(128));
            int16x8_t v16 = vsubq_s16(vreinterpretq_s16_u16(vmovl_u8(vtbl1_u8(uv, vcreate_u8(UINT64_C(0x0707050503030101))))), vdupq_n_s16(128));
            int16x4_t ul = vget_low_s16(u16), uh = vget_high_s16(u16), vl = vget_low_s16(v16), vh = vget_high_s16(v16);
            rfx_vec yl = vmull_n_s16(vget_low_s16(y16), 298), yh = vmull_n_s16(vget_high_s16(y16), 298);
            int16x8_t b = nv12_rgb8(vmlal_n_s16(yl, ul, 541), vmlal_n_s16(yh, uh, 541));
            int16x8_t g = nv12_rgb8(vmlsl_n_s16(vmlsl_n_s16(yl, ul, 55), vl, 136), vmlsl_n_s16(vmlsl_n_s16(yh, uh, 55), vh, 136));
            int16x8_t r = nv12_rgb8(vmlal_n_s16(yl, vl, 459), vmlal_n_s16(yh, vh, 459));
            vstore(e->planes[0] + offset + xx, vsub(vshift(color4(vget_low_s16(b), vget_low_s16(g), vget_low_s16(r), 3735,19235,9798), 10), vconstant(4096)));
            vstore(e->planes[0] + offset + xx + 4, vsub(vshift(color4(vget_high_s16(b), vget_high_s16(g), vget_high_s16(r), 3735,19235,9798), 10), vconstant(4096)));
            vstore(e->planes[1] + offset + xx, vshift(color4(vget_low_s16(b), vget_low_s16(g), vget_low_s16(r), 16403,-10868,-5535), 10));
            vstore(e->planes[1] + offset + xx + 4, vshift(color4(vget_high_s16(b), vget_high_s16(g), vget_high_s16(r), 16403,-10868,-5535), 10));
            vstore(e->planes[2] + offset + xx, vshift(color4(vget_low_s16(b), vget_low_s16(g), vget_low_s16(r), -2663,-13714,16377), 10));
            vstore(e->planes[2] + offset + xx + 4, vshift(color4(vget_high_s16(b), vget_high_s16(g), vget_high_s16(r), -2663,-13714,16377), 10));
        }
#endif
        for (; xx < columns; ++xx) {
            int32_t yv = 298 * ((int32_t)luma[xx] - 16);
            int32_t u = (int32_t)chroma[xx / 2 * 2] - 128, v = (int32_t)chroma[xx / 2 * 2 + 1] - 128;
            int32_t b = clamp255(shift(yv + 541*u + 128, 8));
            int32_t g = clamp255(shift(yv - 55*u - 136*v + 128, 8));
            int32_t r = clamp255(shift(yv + 459*v + 128, 8));
            e->planes[0][offset + xx] = shift(9798*r + 19235*g + 3735*b, 10) - 4096;
            e->planes[1][offset + xx] = shift(-5535*r - 10868*g + 16403*b, 10);
            e->planes[2][offset + xx] = shift(16377*r - 13714*g - 2663*b, 10);
        }
        for (; xx < 64; ++xx) {
            for (unsigned c = 0; c < 3; ++c) e->planes[c][offset + xx] = e->planes[c][offset + columns - 1];
        }
    }
}

void *bk_rfx_open(char *message, size_t capacity) {
    rfx_encoder *e = calloc(1, sizeof(*e));
    if (e) {
        e->quantization = 6;
#if defined(RFX_SSE2) || defined(RFX_NEON)
        e->simd = ANYKVM_RFX_SIMD_BASELINE;
#endif
#if defined(ANYKVM_RFX_AVX2)
        if (avx2_available()) e->simd = ANYKVM_RFX_SIMD_AVX2;
#endif
        e->capacity = 64 * 1024;
        e->output = malloc(e->capacity);
        if (e->output) return e;
        free(e);
    }
    snprintf(message, capacity, "could not allocate encoder context");
    return NULL;
}
void bk_rfx_disable_simd(void *context) {
    (void)bk_rfx_set_simd(context, ANYKVM_RFX_SIMD_SCALAR);
}
int bk_rfx_set_quantization(void *context, int quantization) {
    if (!context || quantization < 6 || quantization > 15) return 0;
    ((rfx_encoder *)context)->quantization = (unsigned)quantization;
    return 1;
}
int bk_rfx_set_simd(void *context, int mode) {
    if (!context) return 0;
    if (mode == ANYKVM_RFX_SIMD_SCALAR) {
        ((rfx_encoder *)context)->simd = mode;
        return 1;
    }
#if defined(RFX_SSE2) || defined(RFX_NEON)
    if (mode == ANYKVM_RFX_SIMD_BASELINE) {
        ((rfx_encoder *)context)->simd = mode;
        return 1;
    }
#endif
#if defined(ANYKVM_RFX_AVX2)
    if (mode == ANYKVM_RFX_SIMD_AVX2 && avx2_available()) {
        ((rfx_encoder *)context)->simd = mode;
        return 1;
    }
#endif
    return 0;
}
const char *bk_rfx_backend(void *context) {
    if (!context || !((rfx_encoder *)context)->simd) return "native C scalar";
#if defined(ANYKVM_RFX_AVX2)
    if (((rfx_encoder *)context)->simd == ANYKVM_RFX_SIMD_AVX2) return "native C AVX2";
#endif
#if defined(RFX_SSE2)
    return "native C SSE2";
#elif defined(RFX_NEON)
    return "native C NEON";
#else
    return "native C scalar";
#endif
}
void bk_rfx_close(void *context) {
    rfx_encoder *e = context;
    if (e) { free(e->output); free(e); }
}

// SRL from MS-RDPEGFX 3.1.8.1.5, not an RLGR variant with K=0.
static void srl(rfx_encoder *e, size_t count, unsigned extra_bits) {
    unsigned kp = 8, maximum = (1u << extra_bits) - 1;
    e->pending = 0; e->used = 0;
    for (size_t i = 0; i < count && !e->failed;) {
        unsigned run = 0;
        while (i < count && e->coeffs[i] == 0) { ++run; ++i; }
        unsigned k = kp >> 3;
        while (run >= (1u << k)) {
            bits(e, 0, 1); run -= 1u << k;
            kp = clamp80(kp + 4); k = kp >> 3;
        }
        bits(e, 1, 1); bits(e, run, k); kp = kp > 6 ? kp - 6 : 0;
        if (i == count) break;
        int32_t value = e->coeffs[i++];
        bits(e, value < 0, 1);
        unsigned magnitude = (unsigned)(value < 0 ? -value : value);
        bits(e, 0, magnitude - 1);
        if (magnitude < maximum) bits(e, 1, 1);
    }
    if (e->used) byte(e, e->pending);
    byte(e, 0); // Required lookahead byte, including an empty SRL stream.
    e->pending = 0; e->used = 0;
}

static void upgrade_component(rfx_encoder *e, const int32_t *base, unsigned extra_bits,
                              size_t length_offset) {
    unsigned mask = (1u << extra_bits) - 1;
    size_t count = 0, start = e->length;
    for (size_t i = 0; i < 4015; ++i) {
        int32_t v = base[i];
        unsigned magnitude = (unsigned)(v < 0 ? -v : v);
        if (magnitude <= mask) e->coeffs[count++] = v;
    }
    srl(e, count, extra_bits);
    patch_word(e, length_offset, (uint16_t)(e->length - start));
    start = e->length;
    // LL3 is already at target precision in FIRST: transmit no raw LL3 bits.
    for (size_t i = 0; i < 4015; ++i) {
        int32_t v = base[i];
        unsigned magnitude = (unsigned)(v < 0 ? -v : v);
        if (magnitude > mask) bits(e, magnitude & mask, extra_bits);
    }
    if (e->used) byte(e, e->pending);
    e->pending = 0; e->used = 0;
    patch_word(e, length_offset + 2, (uint16_t)(e->length - start));
}

int bk_rfx_run_pass(void *context, const uint8_t *pixels, size_t pixel_bytes, int format,
               uint32_t width, uint32_t height, const anykvm_rfx_rect *rects,
               size_t count, uint32_t frame, int first, int pass, unsigned extra_bits,
               int32_t *cache, size_t cache_count, const uint8_t **output,
               size_t *output_bytes, char *message, size_t capacity) {
    rfx_encoder *e = context;
    if (!e || (pass != ANYKVM_RFX_UPGRADE && !pixels) || !rects || !output || !output_bytes || count == 0 || count > ANYKVM_RFX_MAX_TILES ||
        width == 0 || height == 0 || width > 8192 || height > 8192 ||
        (format != ANYKVM_RFX_BGRA && format != ANYKVM_RFX_NV12)) {
        snprintf(message, capacity, "invalid frame or batch"); return 0;
    }
    size_t expected = format == ANYKVM_RFX_BGRA ? (size_t)width * height * 4 :
        (size_t)width * height + 2 * (size_t)((width + 1) / 2) * ((height + 1) / 2);
    if (pass != ANYKVM_RFX_UPGRADE && pixel_bytes != expected) {
        snprintf(message, capacity, "invalid pixel buffer"); return 0;
    }
    size_t required_cache = (size_t)((width + 63) / 64) * ((height + 63) / 64) * 3 * 4096;
    if (pass < ANYKVM_RFX_SIMPLE || pass > ANYKVM_RFX_UPGRADE ||
        (pass != ANYKVM_RFX_SIMPLE && (!cache || cache_count != required_cache || extra_bits < 1 || extra_bits > 4))) {
        snprintf(message, capacity, "invalid progressive pass or coefficient cache"); return 0;
    }
    uint16_t tile_count = 0;
    for (size_t i = 0; i < count; ++i) {
        const anykvm_rfx_rect *r = &rects[i];
        if (!r->width || !r->height || r->width > 128 || r->height > 128 || r->x % 64 || r->y % 64 ||
            (uint32_t)r->x + r->width > width || (uint32_t)r->y + r->height > height) {
            snprintf(message, capacity, "invalid dirty rectangle"); return 0;
        }
        tile_count += (uint16_t)(((r->width + 63) / 64) * ((r->height + 63) / 64));
    }
    if (tile_count > ANYKVM_RFX_MAX_TILES) {
        snprintf(message, capacity, "batch exceeds eight tiles"); return 0;
    }
    e->length = 0; e->failed = 0;
    if (first) {
        block(e, 0xccc0, 12); dword(e, 0xcaccacca); word(e, 0x100);
        block(e, 0xccc3, 10); byte(e, 0); word(e, 64); byte(e, pass != ANYKVM_RFX_SIMPLE);
    }
    // One WIRETOSURFACE_2 payload has one frame and region. A decoder may
    // reject concatenated frames or retain only the last clipping list.
    block(e, 0xccc1, 12); dword(e, frame); word(e, 1);
    size_t region = e->length;
    block(e, 0xccc4, 0);
    byte(e, 64); word(e, (uint16_t)count); byte(e, 1); byte(e, pass == ANYKVM_RFX_FIRST); byte(e, pass != ANYKVM_RFX_SIMPLE);
    word(e, tile_count); dword(e, 0);
    for (size_t i = 0; i < count; ++i) {
        const anykvm_rfx_rect *r = &rects[i];
        word(e, r->x); word(e, r->y); word(e, r->width); word(e, r->height);
    }
    for (unsigned q = 0; q < 5; ++q) byte(e, (uint8_t)(e->quantization | (e->quantization << 4)));
    if (pass == ANYKVM_RFX_FIRST) {
        byte(e, 50);
        // Keep LL3 (first low nibble of each component table) at full precision.
        for (unsigned q = 0; q < 15; ++q)
            byte(e, (uint8_t)((extra_bits << 4) | (q % 5 ? extra_bits : 0)));
    }
    size_t tiles = e->length;
    for (size_t i = 0; i < count && !e->failed; ++i) {
        const anykvm_rfx_rect *r = &rects[i];
        for (unsigned y = r->y; y < (unsigned)r->y + r->height && !e->failed; y += 64) {
            for (unsigned x = r->x; x < (unsigned)r->x + r->width && !e->failed; x += 64) {
                if (pass != ANYKVM_RFX_UPGRADE) {
                    if (format == ANYKVM_RFX_NV12) ict_nv12(e, pixels, width, height, x, y);
                    else ict(e, pixels, width, height, x, y);
                }
                size_t cache_offset = ((size_t)(y / 64) * ((width + 63) / 64) + x / 64) * 3 * 4096;
                size_t tile = e->length;
                block(e, pass == ANYKVM_RFX_SIMPLE ? 0xccc5 : pass == ANYKVM_RFX_FIRST ? 0xccc6 : 0xccc7, 0);
                byte(e, 0); byte(e, 0); byte(e, 0);
                word(e, (uint16_t)(x / 64)); word(e, (uint16_t)(y / 64));
                if (pass != ANYKVM_RFX_UPGRADE) byte(e, 0); // Original tile flags.
                if (pass != ANYKVM_RFX_SIMPLE) byte(e, pass == ANYKVM_RFX_UPGRADE ? 255 : 0);
                for (unsigned c = 0; c < (pass == ANYKVM_RFX_UPGRADE ? 6u : 4u); ++c) word(e, 0);
                for (unsigned c = 0; c < 3 && !e->failed; ++c) {
                    if (pass == ANYKVM_RFX_UPGRADE) {
                        upgrade_component(e, cache + cache_offset + c * 4096, extra_bits, tile + 14 + 4 * c);
                        continue;
                    }
                    if (pass == ANYKVM_RFX_FIRST) {
#if defined(ANYKVM_RFX_AVX2)
                        if (e->simd == ANYKVM_RFX_SIMD_AVX2)
                            bk_rfx_dwt_reduce_avx2(e->planes[c], e->vertical, e->bands, e->coeffs, e->quantization, 1);
                        else
#endif
                            dwt_reduce(e->planes[c], e->vertical, e->bands, e->coeffs, e->quantization, e->simd);
                    } else dwt(e, e->planes[c]);
                    if (pass == ANYKVM_RFX_FIRST) {
                        memcpy(cache + cache_offset + c * 4096, e->coeffs, 4096 * sizeof(int32_t));
                        for (size_t j = 0; j < 4015; ++j) {
                            int32_t v = e->coeffs[j];
                            e->coeffs[j] = v < 0 ? -shift(-v, extra_bits) : shift(v, extra_bits);
                        }
                        for (size_t j = 4095; j > 4015; --j) e->coeffs[j] -= e->coeffs[j - 1];
                    }
                    size_t component = e->length;
                    rlgr(e);
                    size_t length = e->length - component;
                    if (length > UINT16_MAX) e->failed = 1;
                    if (!e->failed) patch_word(e, tile + (pass == ANYKVM_RFX_FIRST ? 15 : 14) + 2 * c, (uint16_t)length);
                }
                if (!e->failed) patch_dword(e, tile + 2, (uint32_t)(e->length - tile));
            }
        }
    }
    if (!e->failed) {
        patch_dword(e, region + 2, (uint32_t)(e->length - region));
        patch_dword(e, region + 14, (uint32_t)(e->length - tiles));
        block(e, 0xccc2, 6);
    }
    if (e->failed) {
        snprintf(message, capacity, "allocation failed or batch exceeds 256 KiB"); return 0;
    }
    *output = e->output; *output_bytes = e->length;
    return 1;
}

int bk_rfx_run_format(void *context, const uint8_t *pixels, size_t pixel_bytes, int format,
               uint32_t width, uint32_t height, const anykvm_rfx_rect *rects,
               size_t count, uint32_t frame, int first, const uint8_t **output,
               size_t *output_bytes, char *message, size_t capacity) {
    return bk_rfx_run_pass(context, pixels, pixel_bytes, format, width, height, rects,
        count, frame, first, ANYKVM_RFX_SIMPLE, 0, NULL, 0, output, output_bytes, message, capacity);
}

int bk_rfx_run(void *context, const uint8_t *pixels, size_t pixel_bytes,
               uint32_t width, uint32_t height, const anykvm_rfx_rect *rects,
               size_t count, uint32_t frame, int first, const uint8_t **output,
               size_t *output_bytes, char *message, size_t capacity) {
    return bk_rfx_run_format(context, pixels, pixel_bytes, ANYKVM_RFX_BGRA, width, height,
        rects, count, frame, first, output, output_bytes, message, capacity);
}
