#ifndef ANYKVM_YCOCG_NATIVE_H
#define ANYKVM_YCOCG_NATIVE_H
#include <stdint.h>
void bk_ycocg420(int avx2, int nv12, uint8_t *dst, const uint8_t *first,
    const uint8_t *uv, int first_stride, int uv_stride, int width, int height);
void bk_planar_delta(int avx2, uint8_t *dst, const uint8_t *row,
    const uint8_t *previous, int size);
#endif
