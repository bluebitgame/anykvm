//go:build cgo && arm64 && (darwin || linux || windows)

/* ARM64 NEON counterparts of the SSE2/AVX2 color and row-difference kernels. */
#include "ycocg_native.h"
#include <arm_neon.h>
#include <string.h>

typedef struct {
    int32x4_t b, g, r;
} rgb4;
static inline int32x4_t clip4(int32x4_t x) {
    return vminq_s32(vmaxq_s32(x, vdupq_n_s32(0)), vdupq_n_s32(255));
}
static inline rgb4 bgra4(const uint8_t *p) {
    uint32x4_t a = vreinterpretq_u32_u8(vld1q_u8(p)), mask = vdupq_n_u32(255);
    return (rgb4){vreinterpretq_s32_u32(vandq_u32(a, mask)),
                  vreinterpretq_s32_u32(vandq_u32(vshrq_n_u32(a, 8), mask)),
                  vreinterpretq_s32_u32(vandq_u32(vshrq_n_u32(a, 16), mask))};
}
static inline int32x4_t uv4(const uint8_t *p) {
    uint32_t value;
    memcpy(&value, p, 4);
    uint16x4_t pairs = vreinterpret_u16_u8(vcreate_u8(value));
    return vreinterpretq_s32_u32(vmovl_u16(vzip1_u16(pairs, pairs)));
}
static inline rgb4 nv4(const uint8_t *p, int32x4_t uv) {
    uint32_t value;
    memcpy(&value, p, 4);
    int32x4_t u = vsubq_s32(vandq_s32(uv, vdupq_n_s32(255)), vdupq_n_s32(128));
    int32x4_t v = vsubq_s32(vshrq_n_s32(uv, 8), vdupq_n_s32(128));
    int32x4_t yy = vreinterpretq_s32_u32(vmovl_u16(vget_low_u16(vmovl_u8(vcreate_u8(value)))));
    yy = vmulq_n_s32(vsubq_s32(yy, vdupq_n_s32(16)), 298);
    const int32x4_t bias = vdupq_n_s32(128);
    return (rgb4){
        clip4(vshrq_n_s32(vaddq_s32(vmlaq_n_s32(yy, u, 541), bias), 8)),
        clip4(vshrq_n_s32(vaddq_s32(vmlaq_n_s32(vmlaq_n_s32(yy, u, -55), v, -136), bias), 8)),
        clip4(vshrq_n_s32(vaddq_s32(vmlaq_n_s32(yy, v, 459), bias), 8))};
}
static inline int32x4_t co4(rgb4 a) {
    return vsubq_s32(a.b, a.r);
}
static inline int32x4_t cg4(rgb4 a) {
    return vsubq_s32(vaddq_s32(a.g, a.g), vaddq_s32(a.r, a.b));
}
static inline void put_y4(uint8_t *p, rgb4 a) {
    int32x4_t y = vshrq_n_s32(vaddq_s32(vaddq_s32(a.r, a.b), vaddq_s32(a.g, a.g)), 2);
    uint8x8_t bytes = vmovn_u16(vcombine_u16(vmovn_u32(vreinterpretq_u32_s32(y)), vdup_n_u16(0)));
    uint32_t value = vget_lane_u32(vreinterpret_u32_u8(bytes), 0);
    memcpy(p, &value, 4);
}
static inline void put_c2(uint8_t *p, int32x4_t a, int32x4_t b, int shift) {
    int32x4_t sum = vaddq_s32(a, b);
    sum = vpaddq_s32(sum, sum);
    sum = vshlq_s32(sum, vdupq_n_s32(-shift));
    int8x8_t bytes = vmovn_s16(vcombine_s16(vmovn_s32(sum), vdup_n_s16(0)));
    uint16_t value = vget_lane_u16(vreinterpret_u16_s8(bytes), 0);
    memcpy(p, &value, 2);
}
static inline int clip(int v) {
    return v < 0 ? 0 : (v > 255 ? 255 : v);
}
static void edge(uint8_t *dst, const uint8_t *first, const uint8_t *uv, int fs, int us, int w,
                 int h, int x, int y, int nv12) {
    int co = 0, cg = 0, cw = (w + 1) / 2, ch = (h + 1) / 2;
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
            co += b - r;
            cg += 2 * g - r - b;
        }
    int i = (y / 2) * cw + x / 2;
    dst[w * h + i] = (uint8_t)(co >> 3);
    dst[w * h + cw * ch + i] = (uint8_t)(cg >> 4);
}
void bk_ycocg420(int unused, int nv12, uint8_t *dst, const uint8_t *first, const uint8_t *uv,
                 int fs, int us, int w, int h) {
    (void)unused;
    const int cw = (w + 1) / 2, ch = (h + 1) / 2;
    for (int y = 0; y < h; y += 2) {
        const int yb = y + 1 < h ? y + 1 : y;
        int x = 0;
        for (; x + 4 <= w; x += 4) {
            rgb4 a, b;
            if (nv12) {
                int32x4_t c = uv4(uv + (y / 2) * us + x);
                a = nv4(first + y * fs + x, c);
                b = nv4(first + yb * fs + x, c);
            } else {
                a = bgra4(first + y * fs + x * 4);
                b = bgra4(first + yb * fs + x * 4);
            }
            const int i = (y / 2) * cw + x / 2;
            put_y4(dst + y * w + x, a);
            put_y4(dst + yb * w + x, b);
            put_c2(dst + w * h + i, co4(a), co4(b), 3);
            put_c2(dst + w * h + cw * ch + i, cg4(a), cg4(b), 4);
        }
        for (; x < w; x += 2)
            edge(dst, first, uv, fs, us, w, h, x, y, nv12);
    }
}
void bk_planar_delta(int unused, uint8_t *dst, const uint8_t *row, const uint8_t *prev, int size) {
    (void)unused;
    int i = 0;
    for (; i + 16 <= size; i += 16) {
        uint8x16_t d = vsubq_u8(vld1q_u8(row + i), vld1q_u8(prev + i));
        uint8x16_t sign = vcltq_s8(vreinterpretq_s8_u8(d), vdupq_n_s8(0));
        vst1q_u8(dst + i, veorq_u8(vaddq_u8(d, d), sign));
    }
    for (; i < size; ++i) {
        int d = (int8_t)(row[i] - prev[i]);
        dst[i] = (uint8_t)(d < 0 ? -2 * d - 1 : 2 * d);
    }
}
