#ifndef ANYKVM_SPICE_NATIVE_BITMAP_H
#define ANYKVM_SPICE_NATIVE_BITMAP_H

#include <stddef.h>
#include <stdint.h>
#include <string.h>

// Copy SPICE's native-endian RGB surface into packed BGRA. Source data points
// at the first display row; signed stride preserves row orientation.
static void bk_spice_bitmap_copy(uint8_t *output, const uint8_t *data,
                                 int width, int height, int stride, int bits) {
  for (int y = 0; y < height; y++) {
    const uint8_t *row = data + (ptrdiff_t)y * stride;
    for (int x = 0; x < width; x++) {
      uint8_t *pixel = output + ((size_t)y * width + x) * 4;
      if (bits == 32) {
        uint32_t rgb;
        memcpy(&rgb, row + x * 4, sizeof(rgb));
        pixel[0] = rgb;
        pixel[1] = rgb >> 8;
        pixel[2] = rgb >> 16;
      } else {
        uint16_t rgb;
        memcpy(&rgb, row + x * 2, sizeof(rgb));
        int green_bits = bits == 16 ? 6 : 5;
        unsigned blue = rgb & 31;
        unsigned green = (rgb >> 5) & ((1u << green_bits) - 1);
        unsigned red = (rgb >> (5 + green_bits)) & 31;
        pixel[0] = (blue << 3) | (blue >> 2);
        pixel[1] =
            (green << (8 - green_bits)) | (green >> (2 * green_bits - 8));
        pixel[2] = (red << 3) | (red >> 2);
      }
      pixel[3] = 255; // the primary desktop is opaque, even for xRGB
    }
  }
}

#endif
