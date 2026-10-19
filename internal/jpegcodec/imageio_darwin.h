#include <CoreGraphics/CoreGraphics.h>
#include <ImageIO/ImageIO.h>

// The caller keeps image (and any borrowed pixel storage) alive until return.
static CFDataRef bk_imageio_jpeg(CGImageRef image, int quality) {
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
    if (!ok && data) { CFRelease(data); data = NULL; }
    return data;
}
