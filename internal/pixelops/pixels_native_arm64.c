//go:build cgo && arm64 && (darwin || linux || windows)

/* ARM64 NEON kernels for opaque BGRA comparisons, runs and BGR packing. */
#include <arm_neon.h>
#include <stdint.h>

int bk_pixels_equal(const uint8_t *a, const uint8_t *b, int stride, int width, int height) {
    for (int y = 0; y < height; ++y) {
        const uint8_t *p = a + y * stride, *q = b + y * stride;
        int x = 0;
        for (; x + 16 <= width; x += 16) {
            uint8x16x4_t u = vld4q_u8(p + x * 4), v = vld4q_u8(q + x * 4);
            uint8x16_t diff =
                vorrq_u8(vorrq_u8(veorq_u8(u.val[0], v.val[0]), veorq_u8(u.val[1], v.val[1])),
                         veorq_u8(u.val[2], v.val[2]));
            if (vmaxvq_u8(diff))
                return 0;
        }
        for (; x < width; ++x)
            if (p[x * 4] != q[x * 4] || p[x * 4 + 1] != q[x * 4 + 1] ||
                p[x * 4 + 2] != q[x * 4 + 2])
                return 0;
    }
    return 1;
}
int bk_pixels_run(const uint8_t *p, int size, int unit) {
    int count = size / unit, i = 0;
    uint8x16_t b = vdupq_n_u8(p[0]), g = unit == 4 ? vdupq_n_u8(p[1]) : b,
               r = unit == 4 ? vdupq_n_u8(p[2]) : b;
    for (; i + 16 <= count; i += 16) {
        uint8x16_t diff;
        if (unit == 4) {
            uint8x16x4_t a = vld4q_u8(p + i * 4);
            diff = vorrq_u8(vorrq_u8(veorq_u8(a.val[0], b), veorq_u8(a.val[1], g)),
                            veorq_u8(a.val[2], r));
        } else
            diff = veorq_u8(vld1q_u8(p + i), b);
        if (vmaxvq_u8(diff)) {
            uint8_t lanes[16];
            vst1q_u8(lanes, diff);
            for (int k = 0; k < 16; ++k)
                if (lanes[k])
                    return i + k;
        }
    }
    for (; i < count; ++i)
        if (p[i * unit] != p[0] || (unit == 4 && (p[i * 4 + 1] != p[1] || p[i * 4 + 2] != p[2])))
            break;
    return i;
}
void bk_pixels_pack(uint8_t *dst, const uint8_t *src, int count) {
    int i = 0;
    for (; i + 16 <= count; i += 16) {
        uint8x16x4_t a = vld4q_u8(src + i * 4);
        uint8x16x3_t b = {{a.val[0], a.val[1], a.val[2]}};
        vst3q_u8(dst + i * 3, b);
    }
    for (; i < count; ++i) {
        dst[i * 3] = src[i * 4];
        dst[i * 3 + 1] = src[i * 4 + 1];
        dst[i * 3 + 2] = src[i * 4 + 2];
    }
}
