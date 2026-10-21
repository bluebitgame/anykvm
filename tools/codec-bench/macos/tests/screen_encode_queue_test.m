// Integration test of the production native screen encoder and queue, without
// accessing the display. Build without ARC, like the capture package.
#import "../../../../internal/capture/screen_darwin.m"
#include <assert.h>
#include <unistd.h>
#include <stdatomic.h>

static atomic_uint notifications;
@interface BKTimingTestStream : NSObject
@property(nonatomic, readonly) CMClockRef synchronizationClock;
@end
@implementation BKTimingTestStream
- (CMClockRef)synchronizationClock { return CMClockGetHostTimeClock(); }
@end

static void test_source_clock_conversion(void) {
    CMClockRef host = CMClockGetHostTimeClock();
    CMTime anchor = CMClockGetTime(host);
    CMTime source = CMTimeAdd(anchor, CMTimeMake(50, 1000));
    CMTimebaseRef shifted = NULL;
    assert(!CMTimebaseCreateWithSourceClock(NULL, host, &shifted));
    assert(!CMTimebaseSetRateAndAnchorTime(shifted, 1, source, anchor));
    long long expected = bk_screen_pts_host_ns(anchor, host);
    assert(llabs(bk_screen_pts_host_ns(source, shifted) - expected) <= 1);
    assert(!bk_screen_pts_host_ns(source, NULL));
    assert(!bk_screen_pts_host_ns(kCMTimeInvalid, shifted));
    assert(!bk_screen_pts_host_ns(kCMTimeIndefinite, shifted));
    CFRelease(shifted);

    // Future source/display references must survive as signed diagnostics, not
    // be clamped to callback time or replaced by a later decoder timestamp.
    bk_screen_frame_context timing = {0};
    timing.acquired = expected;
    timing.present = expected + 5000000;
    timing.display = expected + 10000000;
    NSDictionary *entry = @{@"pts": @(expected + 1000000000),
        @"timing": [NSValue valueWithBytes:&timing objCType:@encode(bk_screen_frame_context)]};
    long long present, display, acquired, submitted, completed, now;
    int repeated;
    bk_screen_lease_timing((void *)entry, &present, &display, &acquired, &submitted, &completed, &now, &repeated);
    assert(present == timing.present && display == timing.display && acquired == expected);
    bk_screen_lease_timing((void *)@{@"pts": @1234}, &present, &display, &acquired,
                          &submitted, &completed, &now, &repeated);
    assert(!present); // Missing source metadata never falls back to playback PTS.
}

static void test_raw_source_timing(void) {
    for (int codec = 0; codec <= 3; codec += 3) {
        BKScreen *s = [BKScreen new];
        s->codec = codec;
        s->width = s->sourceWidth = 320;
        s->height = s->sourceHeight = 240;
        s->timingEnabled = YES;
        CVPixelBufferRef pixel = NULL;
        assert(!CVPixelBufferCreate(NULL, 320, 240, codec
            ? kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange
            : kCVPixelFormatType_32BGRA, NULL, &pixel));
        CMVideoFormatDescriptionRef format = NULL;
        assert(!CMVideoFormatDescriptionCreateForImageBuffer(NULL, pixel, &format));
        CMTime host = CMClockGetTime(CMClockGetHostTimeClock());
        CMTime pts = CMTimeAdd(host, CMTimeMake(1, 1));
        CMSampleTimingInfo timing = {kCMTimeInvalid, pts, kCMTimeInvalid};
        CMSampleBufferRef sample = NULL;
        assert(!CMSampleBufferCreateReadyWithImageBuffer(NULL, pixel, format, &timing, &sample));
        CFArrayRef a = CMSampleBufferGetSampleAttachmentsArray(sample, true);
        CFMutableDictionaryRef info = (CFMutableDictionaryRef)CFArrayGetValueAtIndex(a, 0);
        CFDictionarySetValue(info, (CFStringRef)SCStreamFrameInfoStatus, (CFNumberRef)@(SCFrameStatusComplete));
        CMTime displayTime = CMTimeSubtract(host, CMTimeMake(1, 1000));
        CFDictionarySetValue(info, (CFStringRef)SCStreamFrameInfoDisplayTime,
            (CFNumberRef)@(CMClockConvertHostTimeToSystemUnits(displayTime)));
        BKTimingTestStream *stream = [[[BKTimingTestStream alloc] init] autorelease];
        dispatch_sync(s->queue, ^{
            [s stream:(SCStream *)stream didOutputSampleBuffer:sample ofType:SCStreamOutputTypeScreen];
        });
        void *lease = NULL, *data = NULL, *borrowed = NULL;
        int size, w, h, sw, sh;
        long long age;
        char error[512];
        assert(bk_screen_take_geometry(s, &lease, &data, &borrowed, &size, &w, &h, &sw, &sh,
                                      NULL, &age, error, sizeof(error)) == 1);
        long long present, display, acquired, submitted, completed, now;
        int repeated;
        bk_screen_lease_timing(lease, &present, &display, &acquired, &submitted, &completed, &now, &repeated);
        assert(present == bk_screen_pts_host_ns(pts, CMClockGetHostTimeClock()));
        assert(present > acquired && display < acquired);
        assert(!submitted && !completed && !repeated && borrowed);
        bk_screen_release(lease);
        CFRelease(sample);
        CFRelease(format);
        CFRelease(pixel);
        [s shutdown];
        [s release];
    }
}

static void frame_ready(uintptr_t context) {
    assert(context == 1);
    atomic_fetch_add(&notifications, 1);
}

static void deliver(BKScreen *s, int i, BOOL flush) {
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
    CMTime sourcePTS = CMTimeSubtract(CMClockGetTime(CMClockGetHostTimeClock()), CMTimeMake(5, 1000));
    CMSampleTimingInfo timing = {CMTimeMake(1, 30), sourcePTS, kCMTimeInvalid};
    CMSampleBufferRef sample = NULL;
    assert(!CMSampleBufferCreateReadyWithImageBuffer(NULL, pixel, format, &timing, &sample));
    CFArrayRef a = CMSampleBufferGetSampleAttachmentsArray(sample, true);
    CFDictionarySetValue((CFMutableDictionaryRef)CFArrayGetValueAtIndex(a, 0),
                         (CFStringRef)SCStreamFrameInfoStatus,
                         (CFNumberRef) @(SCFrameStatusComplete));
    CFDictionarySetValue((CFMutableDictionaryRef)CFArrayGetValueAtIndex(a, 0),
                         (CFStringRef)SCStreamFrameInfoDisplayTime,
                         (CFNumberRef) @(CMClockConvertHostTimeToSystemUnits(sourcePTS)));
    dispatch_sync(s->queue, ^{
      BKTimingTestStream *stream = [[[BKTimingTestStream alloc] init] autorelease];
      [s stream:(SCStream *)stream didOutputSampleBuffer:sample ofType:SCStreamOutputTypeScreen];
    });
    if (flush)
        dispatch_sync(s->encodeQueue, ^{ assert(!VTCompressionSessionCompleteFrames(s->encoder, kCMTimeInvalid)); });
    CFRelease(sample);
    CFRelease(format);
    CFRelease(pixel);
    if (flush) {
        [s->lock lock];
        assert(!s->failure && s->inFlight == 0);
        [s->lock unlock];
    }
}
static void submit(BKScreen *s, int i) { deliver(s, i, YES); }

static void test_latest_pending(BKScreen *s) {
    // Hold the encoder worker while the real capture callback continues. This
    // would deadlock if encoding still ran on the callback queue.
    dispatch_semaphore_t entered = dispatch_semaphore_create(0);
    dispatch_semaphore_t resume = dispatch_semaphore_create(0);
    dispatch_async(s->encodeQueue, ^{
        dispatch_semaphore_signal(entered);
        assert(!dispatch_semaphore_wait(resume, dispatch_time(DISPATCH_TIME_NOW, 5*NSEC_PER_SEC)));
    });
    assert(!dispatch_semaphore_wait(entered, dispatch_time(DISPATCH_TIME_NOW, NSEC_PER_SEC)));
    unsigned long long before = s->submitted, replaced = s->pendingReplaced;
    for (int i = 0; i < 3; i++)
        deliver(s, 100+i, NO);
    [s->lock lock];
    assert(s->submitted == before && s->pendingSample && s->encodeWorkerScheduled);
    assert(s->pendingReplaced == replaced+2);
    CMTime latest = CMSampleBufferGetPresentationTimeStamp(s->pendingSample);
    [s->lock unlock];
    dispatch_semaphore_signal(resume);
    dispatch_sync(s->encodeQueue, ^{ assert(!VTCompressionSessionCompleteFrames(s->encoder, kCMTimeInvalid)); });
    [s->lock lock];
    assert(s->submitted == before+1 && !s->pendingSample && !s->encodeWorkerScheduled);
    assert(!CMTimeCompare(s->lastPTS, latest));
    [s->lock unlock];
    dispatch_release(entered);
    dispatch_release(resume);

    // Simulate full asynchronous admission: callbacks must still replace one
    // pending frame, and completion must be able to wake the worker.
    [s->lock lock];
    s->inFlight = s->encoderBufferFrames > 0 ? s->encoderBufferFrames : (s->codec == 2 ? 4 : 2);
    [s->lock unlock];
    deliver(s, 104, NO);
    deliver(s, 105, NO);
    [s->lock lock];
    assert(s->pendingSample && !s->encodeWorkerScheduled);
    latest = CMSampleBufferGetPresentationTimeStamp(s->pendingSample);
    s->inFlight = 0;
    [s scheduleEncoderLocked];
    [s->lock unlock];
    dispatch_sync(s->encodeQueue, ^{ assert(!VTCompressionSessionCompleteFrames(s->encoder, kCMTimeInvalid)); });
    [s->lock lock];
    assert(!s->pendingSample && !s->encodeWorkerScheduled && s->submitted == before+2);
    assert(!CMTimeCompare(s->lastPTS, latest));
    [s->frames removeAllObjects];
    s->forceIDR = YES;
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
    dispatch_sync(s->encodeQueue, ^{ assert(!VTCompressionSessionCompleteFrames(s->encoder, kCMTimeInvalid)); });
    CFRelease(sample);
    assert(s->inFlight == 0 && !s->failure);
}
static void readFrame(BKScreen *s, FILE *out) {
    void *data = NULL, *lease = NULL, *pixel = NULL;
    int size, w, h, sw, sh;
    char error[1024];
    long long age;
    int result = 0;
    for (int tries = 0; tries < 1000 && result == 0; tries++) {
        result = bk_screen_take_geometry(s, &lease, &data, &pixel, &size, &w, &h, &sw, &sh,
                                         NULL, &age, error, sizeof(error));
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
    if (s->timingEnabled) {
        long long present, display, acquired, submitted, completed, now;
        int repeated;
        bk_screen_lease_timing(lease, &present, &display, &acquired, &submitted, &completed, &now, &repeated);
        assert(submitted > 0 && completed >= submitted && now >= completed);
        assert(repeated || (acquired > 0 && submitted >= acquired));
        assert(repeated || present <= acquired); // Source PTS, not adjusted decoder PTS.
        assert(repeated || (present > 0 && display > 0 && llabs(present - display) <= 1));
        assert(!repeated || (!present && !display));
        long long readyStart, readyEnd, imported;
        bk_screen_lease_texture_timing(lease, &readyStart, &readyEnd, &imported);
        assert(repeated || (acquired <= readyStart && readyStart <= readyEnd &&
                            readyEnd <= imported && imported <= submitted));
        assert(!repeated || (!readyStart && !readyEnd && !imported));
    }
    bk_screen_release(lease);
}
int main(int argc, char **argv) {
    @autoreleasepool {
        assert(argc == 2);
        for (int codec = 0; codec <= 3; codec++) {
            assert(CGSizeEqualToSize(bk_screen_output_size(2880, 1800, 2560, 1440, codec, YES, 1440, 900), CGSizeMake(1440, 900)));
            assert(CGSizeEqualToSize(bk_screen_output_size(2880, 1800, 1280, 720, codec, YES, 1440, 900), CGSizeMake(1280, 720)));
            assert(CGSizeEqualToSize(bk_screen_output_size(1280, 1080, 2560, 1440, codec, YES, 1440, 900), CGSizeMake(1280, 900)));
            assert(CGSizeEqualToSize(bk_screen_output_size(1920, 1080, 1280, 720, codec, YES, 0, 0), CGSizeMake(1280, 720)));
            assert(CGSizeEqualToSize(bk_screen_output_size(1920, 1080, 2560, 1440, codec, YES, 0, 0), CGSizeMake(1920, 1080)));
            assert(CGSizeEqualToSize(bk_screen_output_size(1920, 1080, 2560, 720, codec, YES, 0, 0), CGSizeMake(1920, 1080)));
            assert(CGSizeEqualToSize(bk_screen_output_size(1920, 1080, 2560, 1440, codec, NO, 0, 0), CGSizeMake(2560, 1440)));
        }
        assert(CGSizeEqualToSize(bk_screen_output_size(1920, 1080, 1280, 719, 1, YES, 0, 0), CGSizeMake(1280, 720)));
        assert(CGSizeEqualToSize(bk_screen_output_size(2880, 1800, 2560, 1440, 1, YES, 1441, 901), CGSizeMake(1440, 900)));
        test_source_clock_conversion();
        test_raw_source_timing();
        for (int codec = 1; codec <= 2; codec++) {
            for (int attempt = 0; attempt < 2; attempt++) {
                BKScreen *s = [BKScreen new];
                s->codec = codec;
                s->fps = 30;
                s->nativeCadence = attempt == 1; // Cover unspecified frame duration too.
                s->timingEnabled = attempt == 1;
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
                unsigned beforeNotifications = atomic_load(&notifications);
                bk_screen_set_frame_ready(s, frame_ready, 1);
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
                assert(atomic_load(&notifications) >= beforeNotifications + 20);
                test_latest_pending(s);
                dispatch_sync(s->encodeQueue, ^{
                  [s startIdleTimer];
                });
                // No new captures or idle callbacks are needed to keep a static stream alive.
                double before = CFAbsoluteTimeGetCurrent();
                for (int i = 20; i < 40; i++) {
                    readFrame(s, out);
                }
                double elapsed = CFAbsoluteTimeGetCurrent() - before;
                assert(elapsed > 1.15 && elapsed < 2.5); // ~15 fps, not the 30 fps target
                dispatch_sync(s->encodeQueue, ^{
                  [s stopIdleTimer];
                });
                dispatch_sync(s->encodeQueue, ^{ assert(!VTCompressionSessionCompleteFrames(s->encoder, kCMTimeInvalid)); });
                [s->lock lock];
                assert(s->repeatedFrames >= 20);
                [s->frames removeAllObjects];
                [s->lock unlock];
                assert(bk_screen_request_keyframe(s));
                dispatch_sync(s->encodeQueue, ^{});
                dispatch_sync(s->encodeQueue, ^{ assert(!VTCompressionSessionCompleteFrames(s->encoder, kCMTimeInvalid)); });
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
                assert(s->frames.count <= (codec == 2 ? 4 : 2));
                BOOL empty = s->frames.count == 0;
                [s->lock unlock];
                if (empty)
                    submit(s, 40);
                // Late registration must report frames already queued, so the
                // first event cannot be lost while the Go side finishes Open.
                bk_screen_set_frame_ready(s, NULL, 0);
                beforeNotifications = atomic_load(&notifications);
                bk_screen_set_frame_ready(s, frame_ready, 1);
                assert(atomic_load(&notifications) > beforeNotifications);
                snprintf(path, sizeof(path), "%s/%s-recovery-%d.bin", argv[1],
                         codec == 1 ? "h264" : "hevc", attempt);
                out = fopen(path, "wb");
                assert(out);
                readFrame(s, out);
                fclose(out);
                bk_screen_set_frame_ready(s, NULL, 0);
                [s->lock lock];
                s->inFlight = s->codec == 2 ? 4 : 2;
                [s->lock unlock];
                deliver(s, 200, NO); // Teardown releases an unsubmitted retained input.
                assert(s->pendingSample);
                [s shutdown];
                assert(!s->pendingSample && !s->lastPixel && !s->idleTimer);
                [s release];
            }
        }
        puts(
            "native screen encoder: H.264/HEVC complete frames, bounded queue, recovery and teardown passed");
        return 0;
    }
}
