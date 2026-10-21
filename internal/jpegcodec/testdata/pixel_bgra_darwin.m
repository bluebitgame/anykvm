#include "pixel_bgra_darwin.h"
#include <assert.h>
#include <stdlib.h>
#include <string.h>

int main(void) {
    assert(!bk_jpeg_pixel_lock_bgra(NULL));
    // A padded BGRA buffer must be borrowed directly and remain owned by input.
    const size_t width = 17, height = 19, stride = 128;
    uint8_t *bytes = malloc(stride * height);
    memset(bytes, 0x7b, stride * height);
    CVPixelBufferRef input = NULL;
    assert(CVPixelBufferCreateWithBytes(NULL, width, height, kCVPixelFormatType_32BGRA,
        bytes, stride, NULL, NULL, NULL, &input) == kCVReturnSuccess);
    for (int i = 0; i < 3; i++) {
        CVPixelBufferRef bgra = bk_jpeg_pixel_lock_bgra(input);
        assert(bgra == input);
        assert(CVPixelBufferGetBaseAddress(bgra) == bytes);
        assert(CVPixelBufferGetBytesPerRow(bgra) == stride);
        bk_jpeg_pixel_unlock_bgra(bgra);
    }
    CVPixelBufferRelease(input);
    free(bytes);

    // Native capture can supply YUV. Neutral video-range NV12 should become
    // opaque mid-gray BGRA without invoking an ImageIO encoder.
    assert(CVPixelBufferCreate(NULL, 16, 16, kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange,
        NULL, &input) == kCVReturnSuccess);
    assert(CVPixelBufferLockBaseAddress(input, 0) == kCVReturnSuccess);
    for (size_t plane = 0; plane < 2; plane++) {
        memset(CVPixelBufferGetBaseAddressOfPlane(input, plane), 128,
            CVPixelBufferGetBytesPerRowOfPlane(input, plane) * CVPixelBufferGetHeightOfPlane(input, plane));
    }
    CVPixelBufferUnlockBaseAddress(input, 0);
    CVPixelBufferRef bgra = bk_jpeg_pixel_lock_bgra(input);
    assert(bgra && bgra != input);
    assert(CVPixelBufferGetPixelFormatType(bgra) == kCVPixelFormatType_32BGRA);
    const uint8_t *pixels = CVPixelBufferGetBaseAddress(bgra);
    for (size_t y = 0; y < 16; y++) {
        for (size_t x = 0; x < 16; x++) {
            const uint8_t *p = pixels + y * CVPixelBufferGetBytesPerRow(bgra) + x * 4;
            assert(p[0] >= 125 && p[0] <= 135);
            assert(abs(p[0] - p[1]) <= 2 && abs(p[1] - p[2]) <= 2 && p[3] == 255);
        }
    }
    bk_jpeg_pixel_unlock_bgra(bgra);
    CVPixelBufferRelease(input);
    return 0;
}
