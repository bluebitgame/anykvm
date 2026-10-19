// Integration test of the production native screen encoder and queue, without
// accessing the display. Build without ARC, like the capture package.
#import "../../../../internal/capture/screen_darwin.m"
#include <assert.h>
#include <unistd.h>

static void submit(BKScreen *s, int i) {
    CVPixelBufferRef pixel = NULL;
    assert(!CVPixelBufferPoolCreatePixelBuffer(
        NULL, VTCompressionSessionGetPixelBufferPool(s->encoder), &pixel));
    CVPixelBufferLockBaseAddress(pixel, 0);
    for (int plane = 0; plane < 2; plane++) {
        size_t rows = CVPixelBufferGetHeightOfPlane(pixel, plane),
               stride = CVPixelBufferGetBytesPerRowOfPlane(pixel, plane);
        uint8_t *p = CVPixelBufferGetBaseAddressOfPlane(pixel, plane);
        for (size_t y = 0; y < rows; y++)
            memset(p + y * stride, plane ? 128 : 32 + i % 160, CVPixelBufferGetWidth(pixel));
    }
    CVPixelBufferUnlockBaseAddress(pixel, 0);
    CMVideoFormatDescriptionRef format = NULL;
    assert(!CMVideoFormatDescriptionCreateForImageBuffer(NULL, pixel, &format));
    CMSampleTimingInfo timing = {CMTimeMake(1, 30), CMTimeMake(i, 30), kCMTimeInvalid};
    CMSampleBufferRef sample = NULL;
    assert(!CMSampleBufferCreateReadyWithImageBuffer(NULL, pixel, format, &timing, &sample));
    CFArrayRef a = CMSampleBufferGetSampleAttachmentsArray(sample, true);
    CFDictionarySetValue((CFMutableDictionaryRef)CFArrayGetValueAtIndex(a, 0),
                         (CFStringRef)SCStreamFrameInfoStatus,
                         (CFNumberRef) @(SCFrameStatusComplete));
    dispatch_sync(s->queue, ^{
      [s stream:(SCStream *)s didOutputSampleBuffer:sample ofType:SCStreamOutputTypeScreen];
    });
    assert(!VTCompressionSessionCompleteFrames(s->encoder, kCMTimeInvalid));
    CFRelease(sample);
    CFRelease(format);
    CFRelease(pixel);
    [s->lock lock];
    assert(!s->failure && s->inFlight == 0);
    [s->lock unlock];
}
static void idle(BKScreen *s, int i) {
    CMSampleBufferRef sample = NULL;
    assert(!CMSampleBufferCreateReady(NULL, NULL, NULL, 1, 0, NULL, 0, NULL, &sample));
    CFArrayRef a = CMSampleBufferGetSampleAttachmentsArray(sample, true);
    CFDictionarySetValue((CFMutableDictionaryRef)CFArrayGetValueAtIndex(a, 0),
                         (CFStringRef)SCStreamFrameInfoStatus, (CFNumberRef) @(SCFrameStatusIdle));
    // Deliberately no image buffer or valid PTS: exercise the idle fallback.
    dispatch_sync(s->queue, ^{
      [s stream:(SCStream *)s didOutputSampleBuffer:sample ofType:SCStreamOutputTypeScreen];
    });
    assert(!VTCompressionSessionCompleteFrames(s->encoder, kCMTimeInvalid));
    CFRelease(sample);
    assert(s->inFlight == 0 && !s->failure);
}
static void readFrame(BKScreen *s, FILE *out) {
    void *data = NULL;
    int size, w, h, sw, sh;
    char error[1024];
    long long age;
    int result = 0;
    for (int tries = 0; tries < 1000 && result == 0; tries++) {
        result = bk_screen_read(s, &data, &size, &w, &h, &sw, &sh, &age, error, sizeof(error));
        if (!result)
            usleep(1000);
    }
    if (result != 1) {
        bk_screen_idle_status(s, error, sizeof(error));
        fprintf(stderr, "codec=%d no output: %s\n", s->codec, error);
    }
    assert(result == 1);
    assert(w == 320 && h == 240 && size > 4);
    assert(!memcmp(data, "\0\0\0\1", 4));
    assert(fwrite(data, 1, size, out) == size);
    free(data);
}
int main(int argc, char **argv) {
    @autoreleasepool {
        assert(argc == 2);
        for (int codec = 1; codec <= 2; codec++) {
            for (int attempt = 0; attempt < 2; attempt++) {
                BKScreen *s = [BKScreen new];
                s->codec = codec;
                s->fps = 30;
                s->width = 320;
                s->height = 240;
                s->sourceWidth = 320;
                s->sourceHeight = 240;
                NSString *error = nil;
                bk_prepare_encoder(s, 1000, &error);
                if (error) {
                    fprintf(stderr, "%s\n", error.UTF8String);
                    return 1;
                }
                char path[1024];
                snprintf(path, sizeof(path), "%s/%s-%d.bin", argv[1], codec == 1 ? "h264" : "hevc",
                         attempt);
                FILE *out = fopen(path, "wb");
                assert(out);
                idle(s, 0);
                assert(!s->frames.count && s->submitted == 0); // no cached image yet
                for (int i = 0; i < 20; i++) {
                    submit(s, i);
                    readFrame(s, out);
                }
                dispatch_sync(s->queue, ^{
                  [s startIdleTimer];
                });
                // No new captures or idle callbacks are needed to keep a static stream alive.
                double before = CFAbsoluteTimeGetCurrent();
                for (int i = 20; i < 40; i++) {
                    readFrame(s, out);
                }
                double elapsed = CFAbsoluteTimeGetCurrent() - before;
                assert(elapsed > 1.15 && elapsed < 2.5); // ~15 fps, not the 30 fps target
                dispatch_sync(s->queue, ^{
                  [s stopIdleTimer];
                });
                assert(!VTCompressionSessionCompleteFrames(s->encoder, kCMTimeInvalid));
                [s->lock lock];
                assert(s->repeatedFrames >= 20);
                [s->frames removeAllObjects];
                [s->lock unlock];
                assert(bk_screen_request_keyframe(s));
                dispatch_sync(s->queue, ^{
                              });
                assert(!VTCompressionSessionCompleteFrames(s->encoder, kCMTimeInvalid));
                char keypath[1024];
                snprintf(keypath, sizeof(keypath), "%s/%s-request-%d.bin", argv[1],
                         codec == 1 ? "h264" : "hevc", attempt);
                FILE *keyfile = fopen(keypath, "wb");
                assert(keyfile);
                readFrame(s, keyfile);
                fclose(keyfile);
                fclose(out);
                // Force a slow consumer: overflow must wait for an independent keyframe.
                for (int i = 20; i < 40; i++)
                    submit(s, i);
                [s->lock lock];
                assert(s->frames.count <= 4);
                BOOL empty = s->frames.count == 0;
                [s->lock unlock];
                if (empty)
                    submit(s, 40);
                snprintf(path, sizeof(path), "%s/%s-recovery-%d.bin", argv[1],
                         codec == 1 ? "h264" : "hevc", attempt);
                out = fopen(path, "wb");
                assert(out);
                readFrame(s, out);
                fclose(out);
                [s shutdown];
                [s release];
            }
        }
        puts(
            "native screen encoder: H.264/HEVC complete frames, bounded queue, recovery and teardown passed");
        return 0;
    }
}
