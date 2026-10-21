// Exercise both production VideoToolbox paths with synthetic moving NV12.
// No screen capture or input injection. An optional prefix saves Annex-B streams
// for decoding the exact same data on Windows, rather than comparing sessions.
#import "../../../../internal/capture/screen_darwin.m"
#import "../../../../internal/videoenc/native_darwin.m"
#include <assert.h>
#include <unistd.h>

static void single_layer(VTCompressionSessionRef session) {
    CFTypeRef value = NULL;
    assert(!VTSessionCopyProperty(session, kVTCompressionPropertyKey_BaseLayerFrameRateFraction,
                                  NULL, &value));
    assert(value && CFGetTypeID(value) == CFNumberGetTypeID());
    double fraction = 0;
    assert(CFNumberGetValue(value, kCFNumberDoubleType, &fraction));
    CFRelease(value);
    assert(fraction == 1);
    if (@available(macOS 12.0, *)) {
        value = NULL;
        assert(!VTSessionCopyProperty(session, kVTCompressionPropertyKey_EnableLTR, NULL, &value));
        assert(value && CFEqual(value, kCFBooleanFalse));
        CFRelease(value);
    }
    if (bk_h264_single_reference()) {
        if (@available(macOS 13.0, *)) {
            value = NULL;
            int references = 0;
            assert(!VTSessionCopyProperty(session, kVTCompressionPropertyKey_ReferenceBufferCount,
                                          NULL, &value));
            assert(value && CFGetTypeID(value) == CFNumberGetTypeID());
            assert(CFNumberGetValue(value, kCFNumberIntType, &references));
            CFRelease(value);
            assert(references == 1);
        }
    }
}

static CVPixelBufferRef moving_pixel(int frame) {
    CVPixelBufferRef pixel = NULL;
    NSDictionary *attrs = @{(id)kCVPixelBufferIOSurfacePropertiesKey : @{}};
    assert(!CVPixelBufferCreate(NULL, 320, 240, kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange,
                                (CFDictionaryRef)attrs, &pixel));
    assert(!CVPixelBufferLockBaseAddress(pixel, 0));
    uint8_t *luma = CVPixelBufferGetBaseAddressOfPlane(pixel, 0);
    size_t pitch = CVPixelBufferGetBytesPerRowOfPlane(pixel, 0);
    for (int y = 0; y < 240; y++)
        for (int x = 0; x < 320; x++)
            luma[y * pitch + x] = 32 + (((x + frame * 8) / 32 + y / 32) % 2) * 160;
    memset(CVPixelBufferGetBaseAddressOfPlane(pixel, 1), 128,
           CVPixelBufferGetBytesPerRowOfPlane(pixel, 1) * 120);
    CVPixelBufferUnlockBaseAddress(pixel, 0);
    return pixel;
}

static BOOL predictive(const uint8_t *data, size_t size) {
    BOOL p = NO, idr = NO;
    for (size_t i = 0; i + 3 < size; i++)
        if (!data[i] && !data[i + 1] && data[i + 2] == 1) {
            int type = data[i + 3] & 31;
            if (type == 1)
                p = YES;
            if (type == 5)
                idr = YES;
        }
    assert(p || idr);
    return p;
}

static void exercise(int profile, BOOL screen, const char *prefix) {
    BKScreen *s = nil;
    bk_encoder *e = NULL;
    char error[512] = {0};
    if (screen) {
        s = [BKScreen new];
        s->codec = 1;
        s->width = 320;
        s->height = 240;
        s->fps = 60;
        s->h264Profile = profile;
        NSString *message = nil;
        bk_prepare_encoder(s, 2000, &message);
        if (message) {
            fprintf(stderr, "%s\n", message.UTF8String);
            abort();
        }
        single_layer(s->encoder);
    } else {
        e = bk_encoder_open("videotoolbox", "", 320, 240, 60, 1, 2000, 0, 0, profile, error,
                            sizeof(error));
        if (!e) {
            fprintf(stderr, "%s\n", error);
            abort();
        }
        single_layer(e->session);
    }
    FILE *out = NULL;
    const char *profiles[] = {"constrained_baseline", "baseline", "main", "high"};
    if (prefix) {
        char path[4096];
        snprintf(path, sizeof(path), "%s-%s-%s.h264", prefix, screen ? "screen" : "native",
                 profiles[profile]);
        out = fopen(path, "wb");
        assert(out);
    }
    int pframes = 0;
    for (int frame = 0; frame < 96; frame++) {
        CVPixelBufferRef pixel = moving_pixel(frame);
        BOOL key = frame == 0 || frame == 48;
        CMTime pts = CMTimeMake(frame, 60);
        if (screen)
            dispatch_sync(s->encodeQueue, ^{
              if (key)
                  s->forceIDR = YES;
              [s encodePixel:pixel atTime:pts repeat:NO];
            });
        else
            assert(bk_encoder_submit_pixel(e, pixel, frame + 1, key, error, sizeof(error)) == 1);
        CVPixelBufferRelease(pixel);
        void *lease = NULL;
        const uint8_t *data = NULL;
        size_t size = 0;
        BOOL received = NO;
        for (int wait = 0; wait < 5000 && !received; wait++) {
            if (screen) {
                void *bytes = NULL, *buffer = NULL;
                int length, w, h, sw, sh;
                long long age;
                int result = bk_screen_take_geometry(s, &lease, &bytes, &buffer, &length, &w, &h,
                                                     &sw, &sh, NULL, &age, error, sizeof(error));
                assert(result >= 0);
                if (result) {
                    assert(!buffer && w == 320 && h == 240);
                    assert(
                        [((NSDictionary *)lease)[@"pts"] longLongValue] ==
                        CMTimeConvertScale(pts, NSEC_PER_SEC, kCMTimeRoundingMethod_Default).value);
                    data = bytes;
                    size = length;
                    received = YES;
                }
            } else {
                uint64_t id = 0;
                int dropped = 0;
                int result = bk_encoder_receive(e, &lease, &data, &size, &id, &dropped, error,
                                                sizeof(error));
                assert(result >= 0);
                if (result) {
                    assert(id == frame + 1 && !dropped);
                    received = YES;
                }
            }
            if (!received)
                usleep(1000);
        }
        assert(received && size > 4);
        BOOL p = predictive(data, size);
        if (key)
            assert(!p); // Requested recovery is an IDR, not an LTR refresh.
        pframes += p;
        if (out)
            assert(fwrite(data, 1, size, out) == size);
        if (screen)
            bk_screen_release(lease);
        else
            bk_encoder_packet_release(lease);
    }
    assert(pframes > 80); // Reject accidental all-IDR encoding.
    if (screen) {
        assert(s->submitted == 96 && s->encoded == 96 && !s->busyDrops && !s->queueResets);
        [s shutdown];
        [s release];
    } else
        bk_encoder_close(e);
    if (out)
        fclose(out);
    printf(
        "%s %s: single temporal layer, LTR disabled, reference_buffers=%s, 96 ordered frames, IDR recovery, %d P frames\n",
        screen ? "screen" : "native", profiles[profile],
        bk_h264_single_reference() ? "1" : "default", pframes);
}

int main(int argc, char **argv) {
    @autoreleasepool {
        assert(argc <= 2);
        for (int profile = 0; profile < 4; profile++) {
            exercise(profile, NO, argc == 2 ? argv[1] : NULL);
            exercise(profile, YES, argc == 2 ? argv[1] : NULL);
        }
        return 0;
    }
}
