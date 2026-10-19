#import <CoreImage/CoreImage.h>
#import <CoreVideo/CoreVideo.h>

// Tags are shared with capture_darwin.go. Stride/plane padding never escapes.
static size_t bk_pixel_layout(CVPixelBufferRef pixel, int passthrough, int *tag) {
    size_t w = CVPixelBufferGetWidth(pixel), h = CVPixelBufferGetHeight(pixel);
    OSType format = CVPixelBufferGetPixelFormatType(pixel);
    *tag = 1;
    if (passthrough) {
        if (format == kCVPixelFormatType_422YpCbCr8) *tag = 2;
        else if (format == kCVPixelFormatType_422YpCbCr8_yuvs) *tag = 3;
        else if ((format == kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange || format == kCVPixelFormatType_420YpCbCr8BiPlanarFullRange) && CVPixelBufferGetPlaneCount(pixel) == 2) *tag = 4;
    }
    if (*tag == 4) return w * h + 2 * ((w + 1) / 2) * ((h + 1) / 2);
    if (*tag == 2 || *tag == 3) return ((w + 1) / 2) * 4 * h;
    return w * h * 4;
}

static int bk_pixel_copy(CVPixelBufferRef pixel, int tag, uint8_t *dest, size_t size) {
    size_t w = CVPixelBufferGetWidth(pixel), h = CVPixelBufferGetHeight(pixel);
    int expectedTag;
    if (!dest || size != bk_pixel_layout(pixel, tag != 1, &expectedTag) || tag != expectedTag ||
        CVPixelBufferLockBaseAddress(pixel, kCVPixelBufferLock_ReadOnly)) return 0;
    int ok = 1;
    if (tag == 4) {
        size_t rowBytes[] = {w, 2 * ((w + 1) / 2)}, rows[] = {h, (h + 1) / 2};
        for (int plane = 0; plane < 2 && ok; plane++) {
            const uint8_t *src = CVPixelBufferGetBaseAddressOfPlane(pixel, plane);
            size_t stride = CVPixelBufferGetBytesPerRowOfPlane(pixel, plane);
            if (!src || stride < rowBytes[plane] || CVPixelBufferGetHeightOfPlane(pixel, plane) < rows[plane]) { ok = 0; break; }
            for (size_t row = 0; row < rows[plane]; row++) memcpy(dest + row * rowBytes[plane], src + row * stride, rowBytes[plane]);
            dest += rowBytes[plane] * rows[plane];
        }
    } else if (tag != 1 || CVPixelBufferGetPixelFormatType(pixel) == kCVPixelFormatType_32BGRA) {
        const uint8_t *src = CVPixelBufferGetBaseAddress(pixel);
        size_t rowBytes = tag == 1 ? w * 4 : ((w + 1) / 2) * 4;
        size_t stride = CVPixelBufferGetBytesPerRow(pixel);
        if (!src || stride < rowBytes) ok = 0;
        else for (size_t row = 0; row < h; row++) memcpy(dest + row * rowBytes, src + row * stride, rowBytes);
    } else {
        static CIContext *context;
        static CGColorSpaceRef color;
        static dispatch_once_t once;
        dispatch_once(&once, ^{ context = [[CIContext contextWithOptions:nil] retain]; color = CGColorSpaceCreateWithName(kCGColorSpaceSRGB); });
        [context render:[CIImage imageWithCVPixelBuffer:pixel] toBitmap:dest rowBytes:w * 4 bounds:CGRectMake(0, 0, w, h) format:kCIFormatBGRA8 colorSpace:color];
    }
    CVPixelBufferUnlockBaseAddress(pixel, kCVPixelBufferLock_ReadOnly);
    return ok;
}
