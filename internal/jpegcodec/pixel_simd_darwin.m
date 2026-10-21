//go:build darwin && cgo && (amd64 || anykvm_jpeg_simd)

#import <Foundation/Foundation.h>
#import <CoreImage/CoreImage.h>
#include "pixel_simd_darwin.h"

CVPixelBufferRef bk_jpeg_pixel_lock_bgra(CVPixelBufferRef input) {
    @autoreleasepool {
        if (!input) return NULL;
        size_t width = CVPixelBufferGetWidth(input), height = CVPixelBufferGetHeight(input);
        if (!width || !height || width > 8192 || height > 8192) return NULL;
        CVPixelBufferRef pixel = NULL;
        if (CVPixelBufferGetPixelFormatType(input) == kCVPixelFormatType_32BGRA &&
            !CVPixelBufferIsPlanar(input)) {
            pixel = CVPixelBufferRetain(input);
        } else {
            static CIContext *context;
            static CGColorSpaceRef color;
            static dispatch_once_t once;
            dispatch_once(&once, ^{
                context = [[CIContext contextWithOptions:nil] retain];
                color = CGColorSpaceCreateWithName(kCGColorSpaceSRGB);
            });
            if (!context || !color || CVPixelBufferCreate(kCFAllocatorDefault, width, height,
                kCVPixelFormatType_32BGRA, NULL, &pixel) != kCVReturnSuccess) return NULL;
            CIImage *image = [CIImage imageWithCVPixelBuffer:input];
            if (!image) {
                CVPixelBufferRelease(pixel);
                return NULL;
            }
            [context render:image toCVPixelBuffer:pixel bounds:CGRectMake(0, 0, width, height) colorSpace:color];
        }
        if (CVPixelBufferLockBaseAddress(pixel, kCVPixelBufferLock_ReadOnly) != kCVReturnSuccess) {
            CVPixelBufferRelease(pixel);
            return NULL;
        }
        if (!CVPixelBufferGetBaseAddress(pixel) || CVPixelBufferGetBytesPerRow(pixel) < width * 4) {
            bk_jpeg_pixel_unlock_bgra(pixel);
            return NULL;
        }
        return pixel;
    }
}

void bk_jpeg_pixel_unlock_bgra(CVPixelBufferRef pixel) {
    CVPixelBufferUnlockBaseAddress(pixel, kCVPixelBufferLock_ReadOnly);
    CVPixelBufferRelease(pixel);
}
