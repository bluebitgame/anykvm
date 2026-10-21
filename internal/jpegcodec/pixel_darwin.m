//go:build darwin && cgo && !amd64 && !anykvm_jpeg_simd

#import <Foundation/Foundation.h>
#import <CoreVideo/CoreVideo.h>
#import <CoreImage/CoreImage.h>
#include "imageio_darwin.h"

CFDataRef bk_jpeg_encode_pixel(CVPixelBufferRef pixel, int quality) {
    @autoreleasepool {
        if (!pixel || CVPixelBufferLockBaseAddress(pixel, kCVPixelBufferLock_ReadOnly)) return NULL;
        CGImageRef image = NULL;
        CGDataProviderRef provider = NULL;
        CGColorSpaceRef color = CGColorSpaceCreateWithName(kCGColorSpaceSRGB);
        size_t width = CVPixelBufferGetWidth(pixel), height = CVPixelBufferGetHeight(pixel);
        if (CVPixelBufferGetPixelFormatType(pixel) == kCVPixelFormatType_32BGRA && !CVPixelBufferIsPlanar(pixel)) {
            size_t stride = CVPixelBufferGetBytesPerRow(pixel);
            const void *base = CVPixelBufferGetBaseAddress(pixel);
            if (base && stride >= width * 4 && color) {
                provider = CGDataProviderCreateWithData(NULL, base, stride * height, NULL);
                if (provider) image = CGImageCreate(width, height, 8, 32, stride, color,
                    kCGBitmapByteOrder32Little | kCGImageAlphaNoneSkipFirst,
                    provider, NULL, false, kCGRenderingIntentDefault);
            }
        } else {
            // CoreImage converts native YUV without a Go BGRA intermediate.
            static CIContext *context;
            static dispatch_once_t once;
            dispatch_once(&once, ^{ context = [[CIContext contextWithOptions:nil] retain]; });
            CIImage *input = [CIImage imageWithCVPixelBuffer:pixel];
            image = [context createCGImage:input fromRect:CGRectMake(0, 0, width, height) format:kCIFormatBGRA8 colorSpace:color];
        }
        CFDataRef data = bk_imageio_jpeg(image, quality);
        if (image) CGImageRelease(image);
        if (provider) CGDataProviderRelease(provider);
        if (color) CGColorSpaceRelease(color);
        CVPixelBufferUnlockBaseAddress(pixel, kCVPixelBufferLock_ReadOnly);
        return data;
    }
}
