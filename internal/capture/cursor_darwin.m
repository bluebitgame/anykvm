#import <AppKit/AppKit.h>
#import <ApplicationServices/ApplicationServices.h>
#import "cursor_darwin.h"
#include "mainloop_darwin.h"
#include <stdlib.h>
#include <string.h>
#include <math.h>

int cursor_available(void) {
    __block int available = 0;
    void (^read)(void) = ^{
        @autoreleasepool {
            if ([NSApplication sharedApplication] == nil) return;
            [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
            available = 1;
        }
    };
    if (bk_mainloop_active() && ![NSThread isMainThread]) dispatch_sync(dispatch_get_main_queue(), read);
    else read();
    return available;
}

// Rasterization is independent of the live cursor and display queries so that
// Retina dimensions, pixels and hotspots can be verified with synthetic images.
static cursor_result_t *rasterize_cursor(NSImage *img, NSPoint hotspot, int native,
    double scaleX, double scaleY, int sourceWidth, int sourceHeight) {
    NSSize size = [img size];
    int w = native ? (int)lround(size.width * scaleX) : (int)size.width;
    int h = native ? (int)lround(size.height * scaleY) : (int)size.height;
    if (w <= 0 || h <= 0 || w > 256 || h > 256) {
        return NULL;
    }

    size_t bpr = w * 4;
    uint8_t *bgra = (uint8_t *)calloc(1, w * h * 4);
    if (!bgra) return NULL;

    CGColorSpaceRef cs = CGColorSpaceCreateDeviceRGB();
    CGContextRef ctx = CGBitmapContextCreate(bgra, w, h, 8, bpr,
        cs, kCGImageAlphaPremultipliedFirst | kCGBitmapByteOrder32Little);
    CGColorSpaceRelease(cs);
    if (!ctx) { free(bgra); return NULL; }
    if (native) CGContextScaleCTM(ctx, (double)w / size.width, (double)h / size.height);

    NSGraphicsContext *nsCtx = [NSGraphicsContext graphicsContextWithCGContext:ctx flipped:NO];
    [NSGraphicsContext saveGraphicsState];
    [NSGraphicsContext setCurrentContext:nsCtx];
    [img drawAtPoint:NSZeroPoint
            fromRect:NSMakeRect(0, 0, size.width, size.height)
           operation:NSCompositingOperationSourceOver
            fraction:1.0];
    [NSGraphicsContext restoreGraphicsState];
    CGContextRelease(ctx);

    cursor_result_t *cr = (cursor_result_t *)malloc(sizeof(cursor_result_t));
    if (!cr) { free(bgra); return NULL; }
    cr->pixels = bgra;
    cr->width  = w;
    cr->height = h;
    cr->hot_x  = native ? (int)lround(hotspot.x * w / size.width) : (int)hotspot.x;
    cr->hot_y  = native ? (int)lround(hotspot.y * h / size.height) : (int)hotspot.y;
    cr->source_width = sourceWidth;
    cr->source_height = sourceHeight;
    return cr;
}

static cursor_result_t *capture_cursor_image(int native) {
    @autoreleasepool {
    NSCursor *cur = [NSCursor currentSystemCursor];
    if (!cur) {
        return NULL;
    }

    NSImage *img = [cur image];
    if (!img) {
        return NULL;
    }

    NSPoint hotspot = [cur hotSpot];
    double scaleX = 1, scaleY = 1;
    int sourceWidth = 0, sourceHeight = 0;
    if (native) {
        // Quartz locations and mode width/height are logical points. The mode's
        // pixel dimensions include Retina backing scale, even in scaled modes.
        CGDirectDisplayID display = CGMainDisplayID();
        CGEventRef event = CGEventCreate(NULL);
        if (event) {
            uint32_t count = 0;
            CGGetDisplaysWithPoint(CGEventGetLocation(event), 1, &display, &count);
            if (!count) display = CGMainDisplayID();
            CFRelease(event);
        }
        CGDisplayModeRef mode = CGDisplayCopyDisplayMode(display);
        if (mode) {
            sourceWidth = (int)CGDisplayModeGetPixelWidth(mode);
            sourceHeight = (int)CGDisplayModeGetPixelHeight(mode);
            size_t pointsW = CGDisplayModeGetWidth(mode), pointsH = CGDisplayModeGetHeight(mode);
            if (pointsW && pointsH) {
                scaleX = (double)sourceWidth / pointsW;
                scaleY = (double)sourceHeight / pointsH;
            }
            CGDisplayModeRelease(mode);
        }
    }
    return rasterize_cursor(img, hotspot, native, scaleX, scaleY, sourceWidth, sourceHeight);
    }
}

static cursor_result_t *capture_on_main(int native) {
    __block cursor_result_t *result = NULL;
    void (^read)(void) = ^{ result = capture_cursor_image(native); };
    if (bk_mainloop_active() && ![NSThread isMainThread]) dispatch_sync(dispatch_get_main_queue(), read);
    else read();
    return result;
}

cursor_result_t *cursor_capture(void) { return capture_on_main(0); }
cursor_result_t *cursor_capture_native(void) { return capture_on_main(1); }

void cursor_free(cursor_result_t *cr) {
    if (cr) {
        free(cr->pixels);
        free(cr);
    }
}
