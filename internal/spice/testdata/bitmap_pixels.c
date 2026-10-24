#include "internal/spice/native_bitmap.h"
#include <assert.h>
int main(void) {
  uint8_t output[16];
  // xRGB values are native-endian words; row padding must not enter output.
  uint32_t rgb[4] = {0x00112233, 0xdeadbeef, 0x00aabbcc, 0xdeadbeef};
  bk_spice_bitmap_copy(output, (uint8_t *)rgb, 1, 2, 8, 32);
  uint8_t expected[] = {0x33, 0x22, 0x11, 255, 0xcc, 0xbb, 0xaa, 255};
  assert(!memcmp(output, expected, sizeof(expected)));
  bk_spice_bitmap_copy(output, (uint8_t *)&rgb[2], 1, 2, -8, 32);
  assert(!memcmp(output, expected + 4, 4));
  assert(!memcmp(output + 4, expected, 4));
  uint16_t rgb555[] = {0x7c00, 0x03e0, 0x001f, 0x7fff};
  uint8_t primary[] = {0,   0, 255, 255, 0,   255, 0,   255,
                       255, 0, 0,   255, 255, 255, 255, 255};
  bk_spice_bitmap_copy(output, (uint8_t *)rgb555, 4, 1, 8, 15);
  assert(!memcmp(output, primary, sizeof(primary)));
  uint16_t rgb565[] = {0xf800, 0x07e0, 0x001f, 0xffff};
  bk_spice_bitmap_copy(output, (uint8_t *)rgb565, 4, 1, 8, 16);
  assert(!memcmp(output, primary, sizeof(primary)));
  return 0;
}
