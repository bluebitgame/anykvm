//go:build darwin && cgo && !amd64 && !anykvm_jpeg_simd

#import <Foundation/Foundation.h>
#include <CoreGraphics/CoreGraphics.h>
#include <ImageIO/ImageIO.h>
#include "native.h"

CFDataRef bk_jpeg_encode(const uint8_t *pixels, size_t size, int width, int height, int quality) {
    @autoreleasepool {
        CGColorSpaceRef color = CGColorSpaceCreateWithName(kCGColorSpaceSRGB);
        // Borrow BGRA directly; ImageIO finishes synchronously before releasing it.
        CGDataProviderRef provider = CGDataProviderCreateWithData(NULL, pixels, size, NULL);
        CGImageRef image = provider && color ? CGImageCreate(width, height, 8, 32, width * 4,
            color, kCGBitmapByteOrder32Little | kCGImageAlphaNoneSkipFirst,
            provider, NULL, false, kCGRenderingIntentDefault) : NULL;
        CFMutableDataRef data = CFDataCreateMutable(NULL, 0);
        CGImageDestinationRef destination = data ? CGImageDestinationCreateWithData(data, CFSTR("public.jpeg"), 1, NULL) : NULL;
        double value = quality / 100.0;
        CFNumberRef number = CFNumberCreate(NULL, kCFNumberDoubleType, &value);
        const void *keys[] = {kCGImageDestinationLossyCompressionQuality, kCGImageDestinationEmbedThumbnail};
        const void *values[] = {number, kCFBooleanFalse};
        CFDictionaryRef options = number ? CFDictionaryCreate(NULL, keys, values, 2,
            &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks) : NULL;
        bool ok = false;
        if (destination && image && options) {
            CGImageDestinationAddImage(destination, image, options);
            ok = CGImageDestinationFinalize(destination);
        }
        if (options) CFRelease(options);
        if (number) CFRelease(number);
        if (destination) CFRelease(destination);
        if (image) CGImageRelease(image);
        if (provider) CGDataProviderRelease(provider);
        if (color) CGColorSpaceRelease(color);
        if (!ok && data) { CFRelease(data); data = NULL; }
        return data;
    }
}

int bk_jpeg_decode(const uint8_t *data, size_t size, uint8_t *pixels, int width, int height) {
    @autoreleasepool {
        CFDataRef input = CFDataCreateWithBytesNoCopy(NULL, data, size, kCFAllocatorNull);
        CGImageSourceRef source = input ? CGImageSourceCreateWithData(input, NULL) : NULL;
        CGImageRef image = source ? CGImageSourceCreateImageAtIndex(source, 0, NULL) : NULL;
        CGColorSpaceRef color = CGColorSpaceCreateWithName(kCGColorSpaceSRGB);
        CGContextRef context = image && color ? CGBitmapContextCreate(pixels, width, height, 8, width * 4,
            color, kCGBitmapByteOrder32Little | kCGImageAlphaNoneSkipFirst) : NULL;
        int ok = context && CGImageGetWidth(image) == width && CGImageGetHeight(image) == height;
        if (ok) {
            CGContextSetBlendMode(context, kCGBlendModeCopy);
            CGContextDrawImage(context, CGRectMake(0, 0, width, height), image);
            // NoneSkipFirst leaves the alpha byte unspecified; consumers expect opaque.
            for (size_t i = 3; i < (size_t)width * height * 4; i += 4) pixels[i] = 255;
        }
        if (context) CGContextRelease(context);
        if (color) CGColorSpaceRelease(color);
        if (image) CGImageRelease(image);
        if (source) CFRelease(source);
        if (input) CFRelease(input);
        return ok;
    }
}
