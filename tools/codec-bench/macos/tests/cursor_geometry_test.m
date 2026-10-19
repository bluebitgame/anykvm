// Synthetic images exercise production rasterization without reading a screen
// or moving the system pointer. Build with AppKit and ApplicationServices.
#import "../../../../internal/capture/cursor_darwin.m"
#include <assert.h>

int bk_mainloop_active(void) { return 0; }

int main(void) { @autoreleasepool {
    // An 8x12 point image with a high-resolution representation and an opaque
    // red interior surrounded by transparency. A missing CTM scale leaves most
    // of the 2x destination empty, which the far-edge pixel check detects.
    NSBitmapImageRep *rep = [[[NSBitmapImageRep alloc]
        initWithBitmapDataPlanes:NULL pixelsWide:16 pixelsHigh:24 bitsPerSample:8
        samplesPerPixel:4 hasAlpha:YES isPlanar:NO colorSpaceName:NSDeviceRGBColorSpace
        bytesPerRow:64 bitsPerPixel:32] autorelease];
    memset(rep.bitmapData, 0, 16*24*4);
    for (int y=2; y<22; y++) for (int x=2; x<14; x++) {
        unsigned char *pixel = rep.bitmapData + y*64 + x*4;
        pixel[0] = 255; pixel[3] = 255;
    }
    rep.size = NSMakeSize(8,12);
    NSImage *img = [[[NSImage alloc] initWithSize:NSMakeSize(8,12)] autorelease];
    [img addRepresentation:rep];
    for (double scale=1; scale<=2; scale+=0.5) {
        cursor_result_t *cr = rasterize_cursor(img,NSMakePoint(2,4),1,scale,scale,3840,2160);
        assert(cr && cr->width==(int)(8*scale) && cr->height==(int)(12*scale));
        assert(cr->hot_x==(int)(2*scale) && cr->hot_y==(int)(4*scale));
        assert(cr->source_width==3840 && cr->source_height==2160);
        int x=(int)(5*scale), y=(int)(8*scale);
        unsigned char *pixel = cr->pixels + (y*cr->width+x)*4;
        assert(pixel[0]==0 && pixel[1]==0 && pixel[2]>=250 && pixel[3]>=250);
        assert(cr->pixels[3]==0);
        cursor_free(cr);
    }
    cursor_result_t *logical = rasterize_cursor(img,NSMakePoint(2,4),0,1,1,0,0);
    assert(logical && logical->width==8 && logical->height==12);
    assert(logical->hot_x==2 && logical->hot_y==4 && logical->source_width==0);
    cursor_free(logical);
    puts("cursor rasterization: 1x/1.5x/2x dimensions, pixels, transparency, hotspots and logical fallback passed");
    return 0;
}}
