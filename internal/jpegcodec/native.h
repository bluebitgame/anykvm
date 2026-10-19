#include <stdint.h>
#include <stddef.h>
#include <CoreFoundation/CoreFoundation.h>

CFDataRef bk_jpeg_encode(const uint8_t *pixels, size_t size, int width, int height, int quality);
int bk_jpeg_decode(const uint8_t *data, size_t size, uint8_t *pixels, int width, int height);
