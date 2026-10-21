#import <ScreenCaptureKit/ScreenCaptureKit.h>
#import <VideoToolbox/VideoToolbox.h>
#import <CoreGraphics/CoreGraphics.h>
#import <AppKit/AppKit.h>
#include <math.h>
#include <stdio.h>
#include <stdlib.h>
#include "pixels_darwin.h"
#include "../videoenc/reference_darwin.h"

void bk_screen_list_displays(char *out, int outlen) {
    if (!out || outlen <= 0)
        return;
    out[0] = 0;
    CGDirectDisplayID ids[32];
    uint32_t count = 0;
    if (CGGetActiveDisplayList(32, ids, &count) != kCGErrorSuccess)
        return;
    int used = 0;
    for (uint32_t i = 0; i < count && used < outlen - 1; i++) {
        int written = snprintf(out + used, (size_t)(outlen - used), "%u\t%zu\t%zu\n", i,
                               CGDisplayPixelsWide(ids[i]), CGDisplayPixelsHigh(ids[i]));
        if (written < 0 || written >= outlen - used)
            break;
        used += written;
    }
}

@interface BKScreenPixel : NSObject {
  @public
    CVPixelBufferRef pixel;
}
- (id)initWithPixel:(CVPixelBufferRef)value;
@end
@implementation BKScreenPixel
- (id)initWithPixel:(CVPixelBufferRef)value {
    if ((self = [super init]))
        pixel = CVPixelBufferRetain(value);
    return self;
}
- (void)dealloc {
    if (pixel)
        CVPixelBufferRelease(pixel);
    [super dealloc];
}
@end

// Uses manual reference counting. Async completion blocks retain their
// result holder even if the caller times out.
@interface BKScreenResult : NSObject {
  @public
    NSCondition *condition;
    BOOL done;
    id value;
    NSError *error;
}
- (BOOL)wait;
- (void)finish:(id)v error:(NSError *)e;
@end
@implementation BKScreenResult
- (id)init {
    if ((self = [super init]))
        condition = [NSCondition new];
    return self;
}
- (BOOL)wait {
    [condition lock];
    NSDate *deadline = [NSDate dateWithTimeIntervalSinceNow:15];
    while (!done && [condition waitUntilDate:deadline]) {
    }
    BOOL result = done;
    [condition unlock];
    return result;
}
- (void)finish:(id)v error:(NSError *)e {
    [condition lock];
    value = [v retain];
    error = [e retain];
    done = YES;
    [condition broadcast];
    [condition unlock];
}
- (void)dealloc {
    [condition release];
    [value release];
    [error release];
    [super dealloc];
}
@end

typedef struct {
    CGRect rect;
    long long acquired, submitted, completed;
    int repeated;
    long long present;
    long long display;
    // Worker-side boundaries. Readiness is CoreMedia data readiness, not a
    // GPU fence. Import is the native handle handoff, not VT's opaque import.
    long long readyStart, readyEnd, imported;
} bk_screen_frame_context;

static long long bk_screen_host_ns(void) {
    return CMTimeConvertScale(CMClockGetTime(CMClockGetHostTimeClock()), NSEC_PER_SEC,
                              kCMTimeRoundingMethod_Default).value;
}

// Sample PTS belongs to SCStream.synchronizationClock, not implicitly to the
// host clock. Do not estimate an offset from callback arrival (that would
// subtract the very capture delay being measured).
static long long bk_screen_pts_host_ns(CMTime pts, CMClockOrTimebaseRef clock) {
    if (!clock || !CMTIME_IS_NUMERIC(pts))
        return 0;
    CMTime host = CMSyncConvertTime(pts, clock, CMClockGetHostTimeClock());
    if (!CMTIME_IS_NUMERIC(host))
        return 0;
    CMTime ns = CMTimeConvertScale(host, NSEC_PER_SEC, kCMTimeRoundingMethod_Default);
    return CMTIME_IS_NUMERIC(ns) && ns.value > 0 ? ns.value : 0;
}

static bk_screen_frame_context bk_screen_sample_timing(CMSampleBufferRef sample,
                                                       NSDictionary *info,
                                                       CMClockOrTimebaseRef clock,
                                                       long long acquired) {
    bk_screen_frame_context timing = {0};
    timing.acquired = acquired;
    timing.present = bk_screen_pts_host_ns(CMSampleBufferGetPresentationTimeStamp(sample), clock);
    // displayTime is documented in mach absolute units. It describes display,
    // not capture start, and may be scheduled after the callback. Keep it
    // separate so a future display timestamp never becomes capture latency.
    NSNumber *display = info[SCStreamFrameInfoDisplayTime];
    if ([display isKindOfClass:[NSNumber class]] && display.unsignedLongLongValue > 0) {
        CMTime host = CMClockMakeHostTimeFromSystemUnits(display.unsignedLongLongValue);
        timing.display = bk_screen_pts_host_ns(host, CMClockGetHostTimeClock());
    }
    return timing;
}

@interface BKScreen : NSObject <SCStreamOutput, SCStreamDelegate> {
  @public
    SCStream *stream;
    dispatch_queue_t queue;
    dispatch_queue_t encodeQueue;
    VTCompressionSessionRef encoder;
    CMSampleBufferRef pendingSample;
    bk_screen_frame_context pendingTiming;
    BOOL encodeWorkerScheduled, sourceAvailable;
    CVPixelBufferRef lastPixel;
    CGRect lastContentRect;
    CMTime lastPTS;
    dispatch_source_t idleTimer;
    void (*frameReady)(uintptr_t);
    uintptr_t frameReadyContext;
    NSLock *lock;
    NSMutableArray *frames;
    NSString *failure;
    NSMutableData *audio;
    NSString *audioFailure;
    BOOL audioEnabled;
    BOOL closed, forceIDR, awaitingIDR, disableLowLatency, nativeCadence;
    BOOL rdpFullRange, rangeRejected, rangeVerified;
    BOOL timingEnabled;
    bk_screen_frame_context lastCaptureTiming;
    CMFormatDescriptionRef verifiedRangeFormat;
    int h264Profile, encoderBufferFrames;
    int width, height, sourceWidth, sourceHeight, codec, fps, inFlight;
    int lastStatus;
    unsigned long long callbacks, completeFrames, submitted, encoded, busyDrops, queueResets,
        idleFrames, repeatedFrames, pendingReplaced, notReadyDrops;
    unsigned long long encodeCallCount, encodeCallTotalNS, encodeCallMaxNS;
}
- (void)shutdown;
- (void)desktopResumed:(NSNotification *)notification;
- (void)startIdleTimer;
- (void)stopIdleTimer;
- (void)encodePixel:(CVPixelBufferRef)pixel atTime:(CMTime)pts repeat:(BOOL)repeat;
- (void)scheduleEncoderLocked;
- (void)drainPending;
@end

// Called with the native lock held. The Go callback is a nonblocking wakeup;
// it never reads native state or takes the Go device lock.
static void bk_screen_notify_ready(BKScreen *s) {
    if (s->frameReady)
        s->frameReady(s->frameReadyContext);
}

void bk_screen_set_frame_ready(void *handle, void (*notify)(uintptr_t), uintptr_t context) {
    BKScreen *s = handle;
    [s->lock lock];
    s->frameReady = notify;
    s->frameReadyContext = context;
    if (s->failure || s->frames.count)
        bk_screen_notify_ready(s);
    [s->lock unlock];
}

// Only RDP requests full-range encoder input.
static OSType bk_screen_encoder_format(BKScreen *s) {
    return s->rdpFullRange ? kCVPixelFormatType_420YpCbCr8BiPlanarFullRange
                           : kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange;
}

static OSType bk_screen_capture_format(BKScreen *s) {
    return s->codec ? bk_screen_encoder_format(s) : kCVPixelFormatType_32BGRA;
}

static BOOL bk_screen_full_range_output(CMFormatDescriptionRef format) {
    if (!format)
        return NO;
    CFTypeRef full =
        CMFormatDescriptionGetExtension(format, kCMFormatDescriptionExtension_FullRangeVideo);
    CFTypeRef matrix =
        CMFormatDescriptionGetExtension(format, kCMFormatDescriptionExtension_YCbCrMatrix);
    CFTypeRef primaries =
        CMFormatDescriptionGetExtension(format, kCMFormatDescriptionExtension_ColorPrimaries);
    // RDP AVC420 defines BT.709. Older VT encoders omit optional colour_description
    // fields from SPS even when the source and encoder use the requested matrix.
    // Accept omission, but reject any explicitly conflicting colour description.
    return full && CFEqual(full, kCFBooleanTrue) &&
           (!matrix || CFEqual(matrix, kCVImageBufferYCbCrMatrix_ITU_R_709_2)) &&
           (!primaries || CFEqual(primaries, kCVImageBufferColorPrimaries_ITU_R_709_2));
}

static BOOL bk_screen_full_range_bitstream(CMFormatDescriptionRef format) {
    const uint8_t *parameters[2];
    size_t sizes[2];
    for (size_t i = 0; i < 2; i++) {
        if (CMVideoFormatDescriptionGetH264ParameterSetAtIndex(format, i, &parameters[i], &sizes[i],
                                                               NULL, NULL))
            return NO;
    }
    CMVideoFormatDescriptionRef coded = NULL;
    if (CMVideoFormatDescriptionCreateFromH264ParameterSets(NULL, 2, parameters, sizes, 4, &coded))
        return NO;
    BOOL valid = bk_screen_full_range_output(coded);
    CFRelease(coded);
    return valid;
}

// SCK describes the content rectangle in surface points. Convert it to output
// pixels using the frame's point-to-pixel scale, not contentScale (already
// reflected in the rectangle). An empty rectangle asks Go to use the fallback.
static CGRect bk_frame_content_rect(NSDictionary *info, CVPixelBufferRef pixel) {
    CGRect rect;
    id value = info[SCStreamFrameInfoContentRect];
    NSNumber *scale = info[SCStreamFrameInfoScaleFactor];
    if (![value isKindOfClass:[NSDictionary class]] || ![scale isKindOfClass:[NSNumber class]] ||
        !CGRectMakeWithDictionaryRepresentation((CFDictionaryRef)value, &rect))
        return CGRectZero;
    double factor = scale.doubleValue;
    if (!isfinite(factor) || factor <= 0)
        return CGRectZero;
    rect = CGRectApplyAffineTransform(rect, CGAffineTransformMakeScale(factor, factor));
    if (!isfinite(rect.origin.x) || !isfinite(rect.origin.y) || !isfinite(rect.size.width) ||
        !isfinite(rect.size.height))
        return CGRectZero;
    rect = CGRectMake(round(rect.origin.x), round(rect.origin.y), round(rect.size.width),
                      round(rect.size.height));
    CGRect canvas = CGRectMake(0, 0, CVPixelBufferGetWidth(pixel), CVPixelBufferGetHeight(pixel));
    if (rect.size.width < 2 || rect.size.height < 2 || !CGRectContainsRect(canvas, rect))
        return CGRectZero;
    return rect;
}

static void bk_encode_output(void *ref, void *frameRef, OSStatus status, VTEncodeInfoFlags flags,
                             CMSampleBufferRef sample) {
    @autoreleasepool {
        NSValue *frameContext = [(NSValue *)frameRef autorelease];
        BKScreen *s = (BKScreen *)ref;
        bk_screen_frame_context timing = {0};
        NSValue *contentRect = frameContext;
        if (s->timingEnabled) {
            timing.completed = bk_screen_host_ns();
            long long completed = timing.completed;
            [frameContext getValue:&timing size:sizeof(timing)];
            timing.completed = completed;
            contentRect = [NSValue valueWithRect:timing.rect];
        }
        [s->lock lock];
        s->inFlight--;
        [s scheduleEncoderLocked];
        if (s->closed) {
            [s->lock unlock];
            return;
        }
        if (status || !sample || (flags & kVTEncodeInfo_FrameDropped)) {
            s->forceIDR = YES;
            if (status && s->rdpFullRange && !s->rangeVerified)
                s->rangeRejected = YES;
            if (status && !s->failure)
                s->failure = [[NSString
                    stringWithFormat:@"VideoToolbox callback failed: %d", (int)status] copy];
            bk_screen_notify_ready(s);
            [s->lock unlock];
            return;
        }
        // Verify the compressed output, not just the requested input format. Do not
        // forward a frame with mismatched range/colorimetry to the RDP client.
        CMFormatDescriptionRef outputFormat = CMSampleBufferGetFormatDescription(sample);
        if (s->rdpFullRange && (!s->rangeVerified || outputFormat != s->verifiedRangeFormat)) {
            if (!bk_screen_full_range_output(outputFormat) ||
                !bk_screen_full_range_bitstream(outputFormat)) {
                s->rangeRejected = YES;
                if (!s->failure)
                    s->failure = [@"VideoToolbox did not produce full-range BT.709 H.264" copy];
                [s->frames removeAllObjects];
                bk_screen_notify_ready(s);
                [s->lock unlock];
                return;
            }
            if (s->verifiedRangeFormat)
                CFRelease(s->verifiedRangeFormat);
            s->verifiedRangeFormat = (CMFormatDescriptionRef)CFRetain(outputFormat);
            if (!s->rangeVerified) {
                s->rangeVerified = YES;
                fprintf(
                    stderr,
                    "capture: RDP VideoToolbox output verified: full_range=1 matrix=BT.709 primaries=BT.709\n");
            }
        }
        CFArrayRef attachments = CMSampleBufferGetSampleAttachmentsArray(sample, false);
        BOOL key = !attachments || !CFArrayGetCount(attachments) ||
                   !CFDictionaryContainsKey(CFArrayGetValueAtIndex(attachments, 0),
                                            kCMSampleAttachmentKey_NotSync);
        // Never discard a P frame and then deliver frames depending on it. On queue
        // overflow discard the chain and request a fresh IDR before resuming delivery.
        int outputLimit = s->encoderBufferFrames > 0 ? s->encoderBufferFrames : (s->codec == 2 ? 4 : 2);
        if ([s->frames count] >= MIN(4, outputLimit)) {
            s->queueResets++;
            [s->frames removeAllObjects];
            s->awaitingIDR = YES;
            s->forceIDR = YES;
        }
        if (s->awaitingIDR && !key) {
            [s->lock unlock];
            return;
        }
        if (key)
            s->awaitingIDR = NO;
        NSMutableData *out = [NSMutableData data];
        const uint8_t start[] = {0, 0, 0, 1};
        CMFormatDescriptionRef format = CMSampleBufferGetFormatDescription(sample);
        int lengthBytes = 0;
        size_t count = 0;
        OSStatus err;
        if (s->codec == 1)
            err = CMVideoFormatDescriptionGetH264ParameterSetAtIndex(format, 0, NULL, NULL, &count,
                                                                     &lengthBytes);
        else
            err = CMVideoFormatDescriptionGetHEVCParameterSetAtIndex(format, 0, NULL, NULL, &count,
                                                                     &lengthBytes);
        if (key && !err)
            for (size_t i = 0; i < count; i++) {
                const uint8_t *p = NULL;
                size_t n = 0;
                if (s->codec == 1)
                    err = CMVideoFormatDescriptionGetH264ParameterSetAtIndex(format, i, &p, &n,
                                                                             NULL, NULL);
                else
                    err = CMVideoFormatDescriptionGetHEVCParameterSetAtIndex(format, i, &p, &n,
                                                                             NULL, NULL);
                if (err)
                    break;
                [out appendBytes:start length:4];
                [out appendBytes:p length:n];
            }
        CMBlockBufferRef block = CMSampleBufferGetDataBuffer(sample);
        size_t size = block ? CMBlockBufferGetDataLength(block) : 0;
        NSMutableData *avcc = [NSMutableData dataWithLength:size];
        if (!size || CMBlockBufferCopyDataBytes(block, 0, size, [avcc mutableBytes]))
            err = -1;
        const uint8_t *bytes = [avcc bytes];
        size_t pos = 0;
        if (lengthBytes < 1 || lengthBytes > 4)
            err = -1;
        while (!err && pos < size) {
            if (size - pos < (size_t)lengthBytes) {
                err = -1;
                break;
            }
            uint32_t n = 0;
            for (int i = 0; i < lengthBytes; i++)
                n = (n << 8) | bytes[pos++];
            if (!n || n > size - pos) {
                err = -1;
                break;
            }
            [out appendBytes:start length:4];
            [out appendBytes:bytes + pos length:n];
            pos += n;
        }
        if (err) {
            if (!s->failure)
                s->failure = [@"VideoToolbox produced an invalid access unit" copy];
        } else {
            s->encoded++;
            NSDictionary *entry = @{
                @"data" : out,
                @"rect" : contentRect ?: [NSValue valueWithRect:NSZeroRect],
                @"pts" : @(CMTimeConvertScale(CMSampleBufferGetPresentationTimeStamp(sample),
                                              NSEC_PER_SEC, kCMTimeRoundingMethod_Default)
                               .value)
            };
            if (s->timingEnabled) {
                NSMutableDictionary *measured = [[entry mutableCopy] autorelease];
                measured[@"timing"] = [NSValue valueWithBytes:&timing objCType:@encode(bk_screen_frame_context)];
                entry = measured;
            }
            [s->frames addObject:entry];
        }
        bk_screen_notify_ready(s);
        [s->lock unlock];
    }
}

// SCK produces Float32 PCM. Normalize planar/interleaved stereo before passing
// it to Go, retaining no sample-buffer memory after this callback returns.
static void bk_screen_buffer_audio(BKScreen *s, CMSampleBufferRef sample) {
    if (!CMSampleBufferIsValid(sample) || !CMSampleBufferDataIsReady(sample))
        return;
    const AudioStreamBasicDescription *f = CMAudioFormatDescriptionGetStreamBasicDescription(
        CMSampleBufferGetFormatDescription(sample));
    CMItemCount count = CMSampleBufferGetNumSamples(sample);
    if (!f || f->mFormatID != kAudioFormatLinearPCM ||
        !(f->mFormatFlags & kAudioFormatFlagIsFloat) ||
        (f->mFormatFlags & kAudioFormatFlagIsBigEndian) || f->mBitsPerChannel != 32 ||
        f->mSampleRate != 48000 || f->mChannelsPerFrame != 2 || count <= 0 || count > 48000) {
        [s->lock lock];
        if (!s->audioFailure)
            s->audioFailure =
                [@"unsupported ScreenCaptureKit audio format (expected 48kHz stereo Float32)" copy];
        [s->lock unlock];
        return;
    }
    // Copy into owned planes so neither alignment nor the sample's lifetime
    // can affect the audio consumed asynchronously by Go.
    BOOL planar = f->mFormatFlags & kAudioFormatFlagIsNonInterleaved;
    AudioBufferList *buffers = calloc(1, sizeof(AudioBufferList) + sizeof(AudioBuffer));
    if (!buffers)
        return;
    NSMutableData *storage = [NSMutableData dataWithLength:(size_t)count * 8];
    buffers->mNumberBuffers = planar ? 2 : 1;
    for (UInt32 i = 0; i < buffers->mNumberBuffers; i++) {
        buffers->mBuffers[i].mNumberChannels = planar ? 1 : 2;
        buffers->mBuffers[i].mDataByteSize = (UInt32)count * (planar ? 4 : 8);
        buffers->mBuffers[i].mData = (uint8_t *)storage.mutableBytes + (planar ? i * count * 4 : 0);
    }
    OSStatus status =
        CMSampleBufferCopyPCMDataIntoAudioBufferList(sample, 0, (int32_t)count, buffers);
    NSMutableData *pcm = nil;
    if (!status) {
        BOOL valid = planar ? buffers->mNumberBuffers == 2 : buffers->mNumberBuffers == 1;
        for (UInt32 i = 0; valid && i < buffers->mNumberBuffers; i++)
            valid = buffers->mBuffers[i].mData &&
                    buffers->mBuffers[i].mNumberChannels == (planar ? 1 : 2) &&
                    buffers->mBuffers[i].mDataByteSize >=
                        (size_t)count * sizeof(float) * (planar ? 1 : 2);
        if (valid) {
            pcm = [NSMutableData dataWithLength:(size_t)count * 2 * sizeof(float)];
            float *out = pcm.mutableBytes;
            if (!planar)
                memcpy(out, buffers->mBuffers[0].mData, pcm.length);
            else
                for (CMItemCount i = 0; i < count; i++) {
                    out[2 * i] = ((float *)buffers->mBuffers[0].mData)[i];
                    out[2 * i + 1] = ((float *)buffers->mBuffers[1].mData)[i];
                }
        }
    }
    free(buffers);
    if (pcm) {
        [s->lock lock];
        if (!s->closed && s->audioEnabled) {
            // At most 100ms of 48kHz stereo Float32; retain the live edge.
            [s->audio appendData:pcm];
            if (s->audio.length > 38400)
                [s->audio replaceBytesInRange:NSMakeRange(0, s->audio.length - 38400)
                                    withBytes:NULL
                                       length:0];
        }
        [s->lock unlock];
    }
}

// A zero-sized read checks availability without consuming buffered audio.
int bk_screen_audio_read(void *handle, void *out, int capacity, char *error, int error_size) {
    @autoreleasepool {
        BKScreen *s = handle;
        [s->lock lock];
        if (s->closed || s->failure || s->audioFailure || !s->audioEnabled) {
            snprintf(error, error_size, "%s", [(s->audioFailure ?: s->failure ?: @"ScreenCaptureKit audio requires macOS 13 or later") UTF8String]);
            [s->lock unlock];
            return -1;
        }
        int size = MIN((NSUInteger)MAX(0, capacity), s->audio.length);
        size -= size % 8;
        if (size && out) {
            memcpy(out, s->audio.bytes, size);
            [s->audio replaceBytesInRange:NSMakeRange(0, size) withBytes:NULL length:0];
        }
        [s->lock unlock];
        return size;
    }
}

@implementation BKScreen
- (id)init {
    if ((self = [super init])) {
        lock = [NSLock new];
        frames = [NSMutableArray new];
        audio = [NSMutableData new];
        queue = dispatch_queue_create("anykvm.screen", DISPATCH_QUEUE_SERIAL);
        encodeQueue = dispatch_queue_create("anykvm.screen.encode", DISPATCH_QUEUE_SERIAL);
        forceIDR = YES;
        lastStatus = -1;
        lastPTS = kCMTimeInvalid;
        // macOS's distributed unlock notification also covers streams that silently
        // stop while retaining a complete/idle status. It is a best-effort system
        // notification; abnormal frame states are independently handled by Go.
        [[NSDistributedNotificationCenter defaultCenter]
                   addObserver:self
                      selector:@selector(desktopResumed:)
                          name:@"com.apple.screenIsUnlocked"
                        object:nil
            suspensionBehavior:NSNotificationSuspensionBehaviorDeliverImmediately];
        [[[NSWorkspace sharedWorkspace] notificationCenter]
            addObserver:self
               selector:@selector(desktopResumed:)
                   name:NSWorkspaceDidWakeNotification
                 object:nil];
        [[[NSWorkspace sharedWorkspace] notificationCenter]
            addObserver:self
               selector:@selector(desktopResumed:)
                   name:NSWorkspaceSessionDidBecomeActiveNotification
                 object:nil];
    }
    return self;
}
- (void)desktopResumed:(NSNotification *)notification {
    [lock lock];
    if (!closed && !failure)
        failure = [[NSString
            stringWithFormat:@"desktop resumed (%@); restarting capture", notification.name] copy];
    bk_screen_notify_ready(self);
    [lock unlock];
}
- (void)stream:(SCStream *)unused didStopWithError:(NSError *)error {
    [lock lock];
    if (!closed && !failure)
        failure =
            [[NSString stringWithFormat:@"%@ (domain=%@ code=%ld)", error.localizedDescription,
                                        error.domain, (long)error.code] copy];
    bk_screen_notify_ready(self);
    [lock unlock];
}
- (void)stream:(SCStream *)sourceStream
    didOutputSampleBuffer:(CMSampleBufferRef)sample
                   ofType:(SCStreamOutputType)type {
    @autoreleasepool {
        if (@available(macOS 13.0, *)) {
            if (type == SCStreamOutputTypeAudio) {
                bk_screen_buffer_audio(self, sample);
                return;
            }
        }
        if (type != SCStreamOutputTypeScreen || !CMSampleBufferIsValid(sample))
            return;
        long long acquiredNS = timingEnabled ? bk_screen_host_ns() : 0;
        NSArray *a = (NSArray *)CMSampleBufferGetSampleAttachmentsArray(sample, false);
        NSNumber *status = a.count ? a[0][SCStreamFrameInfoStatus] : nil;
        [lock lock];
        callbacks++;
        lastStatus = status ? status.intValue : -1;
        [lock unlock];
        if (!status)
            return;
        if (status.integerValue == SCFrameStatusIdle) {
            [lock lock];
            idleFrames++;
            [lock unlock];
            // A separate deadline repeats the image at the minimum FPS, independent
            // of whether ScreenCaptureKit sends idle callbacks at the target FPS.
            return;
        }
        if (status.integerValue != SCFrameStatusComplete) {
            [lock lock];
            sourceAvailable = NO;
            CMSampleBufferRef stale = pendingSample;
            pendingSample = NULL;
            [lock unlock];
            if (stale)
                CFRelease(stale);
            return;
        }
        CVPixelBufferRef pixel = CMSampleBufferGetImageBuffer(sample);
        if (!pixel)
            return;
        if (rdpFullRange && CVPixelBufferGetPixelFormatType(pixel) !=
                                kCVPixelFormatType_420YpCbCr8BiPlanarFullRange) {
            [lock lock];
            rangeRejected = YES;
            if (!failure)
                failure = [@"ScreenCaptureKit did not deliver full-range NV12" copy];
            [frames removeAllObjects];
            bk_screen_notify_ready(self);
            [lock unlock];
            return;
        }
        CGRect contentRect = bk_frame_content_rect(a[0], pixel);
        [lock lock];
        completeFrames++;
        BOOL stopped = closed || failure;
        [lock unlock];
        if (stopped)
            return;
        bk_screen_frame_context sourceTiming = {0};
        if (timingEnabled) {
            CMClockRef clock = NULL;
            if (@available(macOS 13.0, *))
                clock = sourceStream.synchronizationClock;
            sourceTiming = bk_screen_sample_timing(sample, a[0], clock, acquiredNS);
            sourceTiming.rect = contentRect;
        }
        if (encoder) {
            // Retain the original sample/IOSurface; never read or upload pixels.
            // A single replaceable slot bounds both memory and input age while
            // VT is synchronously blocking its independent worker queue.
            sourceTiming.rect = contentRect;
            CFRetain(sample);
            [lock lock];
            CMSampleBufferRef stale = pendingSample;
            if (!closed && !failure) {
                pendingSample = sample;
                pendingTiming = sourceTiming;
                sourceAvailable = YES;
                if (stale)
                    pendingReplaced++;
                [self scheduleEncoderLocked];
            } else {
                CFRelease(sample);
                stale = NULL;
            }
            [lock unlock];
            if (stale)
                CFRelease(stale);
        } else {
            if (CVPixelBufferGetWidth(pixel) != (size_t)width ||
                CVPixelBufferGetHeight(pixel) != (size_t)height)
                return;
            BKScreenPixel *lease = [[[BKScreenPixel alloc] initWithPixel:pixel] autorelease];
            [lock lock];
            if (!closed) {
                [frames removeAllObjects];
                NSDictionary *entry = @{
                    @"pixel" : lease,
                    @"rect" : [NSValue valueWithRect:contentRect],
                    @"pts" : @(CMTimeConvertScale(CMSampleBufferGetPresentationTimeStamp(sample),
                                                  NSEC_PER_SEC, kCMTimeRoundingMethod_Default)
                                   .value)
                };
                if (timingEnabled) {
                    NSMutableDictionary *measured = [[entry mutableCopy] autorelease];
                    measured[@"timing"] = [NSValue valueWithBytes:&sourceTiming objCType:@encode(bk_screen_frame_context)];
                    entry = measured;
                }
                [frames addObject:entry];
                bk_screen_notify_ready(self);
            }
            [lock unlock];
        }
    }
}
// lock must be held. At most one drain task exists, regardless of callback rate.
- (void)scheduleEncoderLocked {
    int limit = encoderBufferFrames > 0 ? encoderBufferFrames : (codec == 2 ? 4 : 2);
    if (!closed && !failure && pendingSample && !encodeWorkerScheduled && inFlight < limit) {
        encodeWorkerScheduled = YES;
        dispatch_async(encodeQueue, ^{ [self drainPending]; });
    }
}
- (void)drainPending {
    for (;;) {
        @autoreleasepool {
            [lock lock];
            int limit = encoderBufferFrames > 0 ? encoderBufferFrames : (codec == 2 ? 4 : 2);
            if (closed || failure || !pendingSample || inFlight >= limit) {
                encodeWorkerScheduled = NO;
                [lock unlock];
                return;
            }
            CMSampleBufferRef sample = pendingSample;
            pendingSample = NULL; // transfer the slot's retain to this worker
            bk_screen_frame_context timing = pendingTiming;
            [lock unlock];
            if (timingEnabled)
                timing.readyStart = bk_screen_host_ns();
            OSStatus readyStatus = noErr;
            if (!CMSampleBufferDataIsReady(sample))
                readyStatus = CMSampleBufferMakeDataReady(sample);
            if (timingEnabled)
                timing.readyEnd = bk_screen_host_ns();
            if (readyStatus || !CMSampleBufferDataIsReady(sample)) {
                [lock lock];
                notReadyDrops++;
                [lock unlock];
                CFRelease(sample);
                continue; // independent input; no encoded prediction was lost
            }
            CVPixelBufferRef pixel = CMSampleBufferGetImageBuffer(sample);
            if (pixel) {
                CVPixelBufferRetain(pixel);
                if (lastPixel)
                    CVPixelBufferRelease(lastPixel);
                lastPixel = pixel;
                lastContentRect = timing.rect;
                if (timingEnabled)
                    timing.imported = bk_screen_host_ns();
                lastCaptureTiming = timing;
                [self encodePixel:pixel atTime:CMSampleBufferGetPresentationTimeStamp(sample) repeat:NO];
            }
            CFRelease(sample);
        }
    }
}
// Runs only on encodeQueue. VT retains submitted pixels through completion.
- (void)encodePixel:(CVPixelBufferRef)pixel atTime:(CMTime)pts repeat:(BOOL)repeat {
    [lock lock];
    if (closed || failure) {
        [lock unlock];
        return;
    }
    // HEVC hardware can withhold two frames before producing output. A two-frame
    // cap would deadlock that pipeline; H.264 low-latency mode keeps its tighter cap.
    if (inFlight >= (encoderBufferFrames > 0 ? encoderBufferFrames : (codec == 2 ? 4 : 2))) {
        busyDrops++;
        [lock unlock];
        return;
    }
    if (!CMTIME_IS_NUMERIC(pts))
        pts = CMClockGetTime(CMClockGetHostTimeClock());
    if (CMTIME_IS_NUMERIC(lastPTS) && CMTimeCompare(pts, lastPTS) <= 0)
        pts = CMTimeAdd(lastPTS, CMTimeMake(1, fps));
    lastPTS = pts;
    BOOL key = forceIDR;
    forceIDR = NO;
    inFlight++;
    submitted++;
    if (repeat)
        repeatedFrames++;
    [lock unlock];
    // Sunshine's default minimum_fps_target is target FPS / 2. New captures
    // postpone this deadline, so motion is never delayed by the idle scheduler.
    if (idleTimer)
        dispatch_source_set_timer(idleTimer,
                                  dispatch_time(DISPATCH_TIME_NOW, 2 * NSEC_PER_SEC / fps),
                                  DISPATCH_TIME_FOREVER, NSEC_PER_MSEC);
    NSDictionary *props = key ? @{(id)kVTEncodeFrameOptionKey_ForceKeyFrame : @YES} : nil;
    bk_screen_frame_context timing = {0};
    if (!repeat)
        timing = lastCaptureTiming;
    timing.rect = lastContentRect;
    timing.repeated = repeat;
    if (timingEnabled)
        timing.submitted = bk_screen_host_ns();
    NSValue *contentRect = [(timingEnabled
        ? [NSValue valueWithBytes:&timing objCType:@encode(bk_screen_frame_context)]
        : [NSValue valueWithRect:lastContentRect]) retain];
    OSStatus status = VTCompressionSessionEncodeFrame(encoder, pixel, pts,
                                                      nativeCadence ? kCMTimeInvalid : CMTimeMake(1, fps),
                                                      (CFDictionaryRef)props, contentRect, NULL);
    if (timingEnabled) {
        unsigned long long elapsed = bk_screen_host_ns() - timing.submitted;
        [lock lock];
        encodeCallCount++;
        encodeCallTotalNS += elapsed;
        encodeCallMaxNS = MAX(encodeCallMaxNS, elapsed);
        [lock unlock];
    }
    if (status) {
        [contentRect release];
        [lock lock];
        inFlight--;
        if (rdpFullRange && !rangeVerified)
            rangeRejected = YES;
        if (!failure)
            failure =
                [[NSString stringWithFormat:@"VideoToolbox encode failed: %d", (int)status] copy];
        bk_screen_notify_ready(self);
        [lock unlock];
    }
}
- (void)startIdleTimer {
    if (!encoder || idleTimer)
        return;
    idleTimer = dispatch_source_create(DISPATCH_SOURCE_TYPE_TIMER, 0, 0, encodeQueue);
    dispatch_source_set_event_handler(idleTimer, ^{
      @autoreleasepool {
          [lock lock];
          BOOL active = !closed && !failure && sourceAvailable && !pendingSample &&
                        (lastStatus == SCFrameStatusComplete || lastStatus == SCFrameStatusIdle);
          [lock unlock];
          if (active && lastPixel)
              [self encodePixel:lastPixel
                         atTime:CMClockGetTime(CMClockGetHostTimeClock())
                         repeat:YES];
          // If encoding was busy, retry after another interval instead of spinning.
          if (idleTimer)
              dispatch_source_set_timer(idleTimer,
                                        dispatch_time(DISPATCH_TIME_NOW, 2 * NSEC_PER_SEC / fps),
                                        DISPATCH_TIME_FOREVER, NSEC_PER_MSEC);
      }
    });
    dispatch_source_set_timer(idleTimer, dispatch_time(DISPATCH_TIME_NOW, 2 * NSEC_PER_SEC / fps),
                              DISPATCH_TIME_FOREVER, NSEC_PER_MSEC);
    dispatch_resume(idleTimer);
}
- (void)stopIdleTimer {
    if (idleTimer) {
        dispatch_source_cancel(idleTimer);
        dispatch_release(idleTimer);
        idleTimer = NULL;
    }
}
- (void)shutdown {
    [lock lock];
    closed = YES;
    frameReady = NULL;
    CMSampleBufferRef stale = pendingSample;
    pendingSample = NULL;
    [lock unlock];
    if (stale)
        CFRelease(stale);
    [[NSDistributedNotificationCenter defaultCenter] removeObserver:self];
    [[[NSWorkspace sharedWorkspace] notificationCenter] removeObserver:self];
    if (stream) {
        BKScreenResult *result = [[[BKScreenResult alloc] init] autorelease];
        [stream stopCaptureWithCompletionHandler:^(NSError *e) {
          [result finish:nil error:e];
        }];
        [result wait];
        [stream removeStreamOutput:self type:SCStreamOutputTypeScreen error:NULL];
        if (@available(macOS 13.0, *))
            if (audioEnabled)
                [stream removeStreamOutput:self type:SCStreamOutputTypeAudio error:NULL];
    }
    dispatch_sync(queue, ^{}); // no more capture callbacks may enqueue input
    dispatch_sync(encodeQueue, ^{
      [self stopIdleTimer];
      if (lastPixel) {
          CVPixelBufferRelease(lastPixel);
          lastPixel = NULL;
      }
    });
    if (encoder) {
        VTCompressionSessionInvalidate(encoder);
        CFRelease(encoder);
        encoder = NULL;
    }
    [stream release];
    stream = nil;
}
- (void)dealloc {
    if (verifiedRangeFormat)
        CFRelease(verifiedRangeFormat);
    [audio release];
    [audioFailure release];
    [lock release];
    [frames release];
    [failure release];
    dispatch_release(queue);
    dispatch_release(encodeQueue);
    [super dealloc];
}
@end

static BOOL bk_property(BKScreen *s, CFStringRef key, CFTypeRef value, NSString **error) {
    OSStatus status = VTSessionSetProperty(s->encoder, key, value);
    if (status)
        *error = [NSString stringWithFormat:@"VideoToolbox %@ failed: %d", key, (int)status];
    return !status;
}

static void bk_prepare_encoder(BKScreen *s, int bitrate, NSString **message) {
    // Default to Constrained Baseline; allow alternate H.264 profiles for decoder compatibility.
    // Keep frame reordering disabled independently of the selected profile.
    CFStringRef h264Profile = kVTProfileLevel_H264_ConstrainedBaseline_AutoLevel;
    switch (s->h264Profile) {
    case 1:
        h264Profile = kVTProfileLevel_H264_Baseline_AutoLevel;
        break;
    case 2:
        h264Profile = kVTProfileLevel_H264_Main_AutoLevel;
        break;
    case 3:
        h264Profile = kVTProfileLevel_H264_High_AutoLevel;
        break;
    }
    NSMutableDictionary *spec = [NSMutableDictionary
        dictionaryWithObject:@YES
                      forKey:
                          (id)kVTVideoEncoderSpecification_RequireHardwareAcceleratedVideoEncoder];
    if (s->codec == 1 && !s->disableLowLatency)
        spec[(id)kVTVideoEncoderSpecification_EnableLowLatencyRateControl] = @YES;
    NSDictionary *attrs = @{
        (id)kCVPixelBufferPixelFormatTypeKey : @(bk_screen_encoder_format(s)),
        (id)kCVPixelBufferIOSurfacePropertiesKey : @{}
    };
    OSStatus status = VTCompressionSessionCreate(
        NULL, s->width, s->height, s->codec == 1 ? kCMVideoCodecType_H264 : kCMVideoCodecType_HEVC,
        (CFDictionaryRef)spec, (CFDictionaryRef)attrs, NULL, bk_encode_output, s, &s->encoder);
    if (status)
        *message =
            [NSString stringWithFormat:@"hardware VideoToolbox creation failed: %d", (int)status];
    else {
        BOOL configured =
            bk_property(s, kVTCompressionPropertyKey_RealTime, kCFBooleanTrue, message) &&
            bk_property(s, kVTCompressionPropertyKey_AllowFrameReordering, kCFBooleanFalse,
                        message) &&
            bk_property(s, kVTCompressionPropertyKey_ProfileLevel,
                        s->codec == 1 ? h264Profile : kVTProfileLevel_HEVC_Main_AutoLevel, message);
        if (configured && s->rdpFullRange)
            configured = bk_property(s, kVTCompressionPropertyKey_ColorPrimaries,
                                     kCVImageBufferColorPrimaries_ITU_R_709_2, message) &&
                         bk_property(s, kVTCompressionPropertyKey_YCbCrMatrix,
                                     kCVImageBufferYCbCrMatrix_ITU_R_709_2, message);
        // The range correction preserves desktop RGB values. Do not require
        // VT's optional sRGB transfer-function property: older hardware rejects
        // it even when full-range NV12 and BT.709 encoding are supported.
        if (configured && bitrate > 0)
            configured = bk_property(s, kVTCompressionPropertyKey_AverageBitRate,
                                     (CFNumberRef) @((int64_t)bitrate * 1000), message);
        if (configured &&
            bk_property(s, kVTCompressionPropertyKey_ExpectedFrameRate, (CFNumberRef) @(s->fps),
                        message) &&
            bk_property(s, kVTCompressionPropertyKey_MaxKeyFrameInterval, (CFNumberRef) @(s->fps),
                        message)) {
            if (s->codec == 1 && !s->disableLowLatency) {
                // GameStream has no VT LTR acknowledgement or temporal-layer negotiation.
                // Keep low-latency rate control, but generate a single temporal layer with
                // short-term references for Windows hardware decoder interoperability.
                if (!bk_property(s, kVTCompressionPropertyKey_BaseLayerFrameRateFraction,
                                 (CFNumberRef) @1.0, message))
                    return;
                if (@available(macOS 12.0, *)) {
                    if (!bk_property(s, kVTCompressionPropertyKey_EnableLTR, kCFBooleanFalse,
                                     message))
                        return;
                }
            }
            if (s->codec == 1) {
                status = bk_h264_reference_buffers(s->encoder);
                if (status) {
                    *message =
                        [NSString stringWithFormat:@"VideoToolbox ReferenceBufferCount failed: %d",
                                                   (int)status];
                    return;
                }
            }
            status = VTCompressionSessionPrepareToEncodeFrames(s->encoder);
            if (status)
                *message =
                    [NSString stringWithFormat:@"VideoToolbox preparation failed: %d", (int)status];
        }
    }
}

static CGSize bk_screen_output_size(int sw, int sh, int w, int h, int codec, BOOL downscaleOnly,
                                    int maxW, int maxH) {
    if (downscaleOnly) {
        // AVC420 padding must fit even when the Target ceiling is odd.
        if (codec == 1) { maxW &= ~1; maxH &= ~1; }
        if (maxW > 0) sw = MIN(sw, maxW);
        if (maxH > 0) sh = MIN(sh, maxH);
    }
    w = w > 0 ? w : sw;
    h = h > 0 ? h : sh;
    if (downscaleOnly && (w > sw || h > sh)) {
        w = sw;
        h = sh;
    }
    // Match the even RDP AVC420 canvas, retaining other formats' alignment.
    if (codec == 1) {
        w = (w + 1) & ~1;
        h = (h + 1) & ~1;
    } else if (codec) {
        w &= ~1;
        h &= ~1;
    }
    return CGSizeMake(w, h);
}

void *bk_screen_open(int index, unsigned int display_id, int w, int h, int fps, int codec,
                     int bitrate, int hide, int preserve_aspect, int disable_low_latency,
                     int h264_profile, int encoder_buffer_frames, int rdp_full_range,
                     int capture_audio, int downscale_only, int max_w, int max_h,
                     char *error, int error_size) {
    @autoreleasepool {
        NSString *message = nil;
        if (@available(macOS 12.3, *)) {
            BKScreenResult *result = [[[BKScreenResult alloc] init] autorelease];
            [SCShareableContent
                getShareableContentExcludingDesktopWindows:YES
                                       onScreenWindowsOnly:YES
                                         completionHandler:^(SCShareableContent *content,
                                                             NSError *err) {
                                           [result finish:content error:err];
                                         }];
            if (![result wait])
                message = @"display enumeration timed out; check Screen Recording permission";
            else if (result->error)
                message = [result->error localizedDescription];
            else {
                SCShareableContent *content = result->value;
                // Use CoreGraphics active display order, consistent with screen:N and cursor mapping.
                CGDirectDisplayID ids[32];
                uint32_t count = 0;
                CGGetActiveDisplayList(32, ids, &count);
                SCDisplay *display = nil;
                if (!display_id && index >= 0 && (uint32_t)index < count)
                    display_id = ids[index];
                for (SCDisplay *d in content.displays)
                    if (d.displayID == display_id) {
                        display = d;
                        break;
                    }
                if (!display)
                    message = @"display index is unavailable";
                else {
                    BKScreen *s = [BKScreen new];
                    s->codec = codec;
                    const char *timingFlag = getenv("ANYKVM_VIDEO_TIMING");
                    s->timingEnabled = timingFlag && timingFlag[0] == '1' && timingFlag[1] == '\0';
                    s->nativeCadence = fps == 0;
                    // A nominal rate is needed for encoder hints and idle repeats.
                    // Motion is driven directly by SCK callbacks, without this cap.
                    s->fps = fps > 0 ? fps : 60;
                    s->rdpFullRange = rdp_full_range != 0 && codec == 1;
                    s->encoderBufferFrames = encoder_buffer_frames;
                    // CGDisplayPixelsWide/High report logical size on Retina. Preserve the
                    // source's backing pixels when no output dimensions were configured.
                    CGDisplayModeRef mode = CGDisplayCopyDisplayMode(display.displayID);
                    s->sourceWidth = mode ? (int)CGDisplayModeGetPixelWidth(mode)
                                          : (int)CGDisplayPixelsWide(display.displayID);
                    s->sourceHeight = mode ? (int)CGDisplayModeGetPixelHeight(mode)
                                           : (int)CGDisplayPixelsHigh(display.displayID);
                    if (mode)
                        CGDisplayModeRelease(mode);
                    CGSize output = bk_screen_output_size(s->sourceWidth, s->sourceHeight,
                                                          w, h, codec, downscale_only, max_w, max_h);
                    s->width = (int)output.width;
                    s->height = (int)output.height;
                    if (s->width < 2 || s->height < 2 || s->width > 8192 || s->height > 8192)
                        message = @"unsupported output dimensions";
                    if ((codec == 1 || codec == 2) && !message) {
                        s->disableLowLatency = disable_low_latency != 0;
                        s->h264Profile = h264_profile;
                        bk_prepare_encoder(s, bitrate, &message);
                    }
                    if (!message) {
                        SCContentFilter *filter =
                            [[[SCContentFilter alloc] initWithDisplay:display
                                                     excludingWindows:@[]] autorelease];
                        SCStreamConfiguration *config =
                            [[[SCStreamConfiguration alloc] init] autorelease];
                        config.width = s->width;
                        config.height = s->height;
                        config.minimumFrameInterval = fps > 0 ? CMTimeMake(1, fps) : kCMTimeZero;
                        config.queueDepth = codec == 2 ? 6 : 3;
                        if (codec == 1 || codec == 2) {
                            int limit = encoder_buffer_frames > 0 ? encoder_buffer_frames : (codec == 2 ? 4 : 2);
                            // Reserve IOSurfaces for in-flight encoding, the latest
                            // pending sample and its replacement. Pending work is
                            // still one slot; pool starvation must not block capture.
                            config.queueDepth = MIN(8, MAX(3, limit + 2));
                        }
                        config.pixelFormat = bk_screen_capture_format(s);
                        if (codec == 3 || s->rdpFullRange)
                            config.colorMatrix = kCGDisplayStreamYCbCrMatrix_ITU_R_709_2;
                        if (s->rdpFullRange)
                            config.colorSpaceName = kCGColorSpaceSRGB;
                        config.showsCursor = !hide;
                        config.scalesToFit = YES;
                        // GameStream maps input using the padded source aspect. VNC/raw consumers
                        // retain their previous full-canvas scaling and coordinate mapping.
                        if (@available(macOS 14.0, *))
                            config.preservesAspectRatio = preserve_aspect;
                        if (preserve_aspect) {
                            double scale = fmin((double)s->width / s->sourceWidth,
                                                (double)s->height / s->sourceHeight);
                            int dw = (int)(s->sourceWidth * scale) & ~1,
                                dh = (int)(s->sourceHeight * scale) & ~1;
                            config.destinationRect =
                                CGRectMake((s->width - dw) / 2, (s->height - dh) / 2, dw, dh);
                        }
                        config.backgroundColor = CGColorGetConstantColor(kCGColorBlack);
                        if (@available(macOS 13.0, *)) {
                            config.capturesAudio = capture_audio != 0;
                            config.excludesCurrentProcessAudio = YES;
                            config.sampleRate = 48000;
                            config.channelCount = 2;
                        }
                        s->stream = [[SCStream alloc] initWithFilter:filter
                                                       configuration:config
                                                            delegate:s];
                        NSError *err = nil;
                        if (![s->stream addStreamOutput:s
                                                   type:SCStreamOutputTypeScreen
                                     sampleHandlerQueue:s->queue
                                                  error:&err])
                            message = [err localizedDescription] ?: @"cannot attach screen output";
                        else {
                            if (@available(macOS 13.0, *))
                                if (capture_audio) {
                                    s->audioEnabled =
                                        [s->stream addStreamOutput:s
                                                              type:SCStreamOutputTypeAudio
                                                sampleHandlerQueue:s->queue
                                                             error:&err];
                                    if (!s->audioEnabled)
                                        s->audioFailure = [[err localizedDescription] copy];
                                }
                            BKScreenResult *started = [[[BKScreenResult alloc] init] autorelease];
                            [s->stream startCaptureWithCompletionHandler:^(NSError *err) {
                              [started finish:nil error:err];
                            }];
                            if (![started wait])
                                message = @"starting screen capture timed out";
                            else if (started->error)
                                message = [started->error localizedDescription];
                        }
                    }
                    if (!message) {
                        dispatch_sync(s->encodeQueue, ^{
                          [s startIdleTimer];
                        });
                        return s;
                    }
                    // Keep error text alive while shutdown waits for asynchronous completions.
                    message = [[message copy] autorelease];
                    [s shutdown];
                    [s release];
                }
            }
        } else
            message = @"macOS 12.3 or later is required";
        snprintf(error, error_size, "%s", [message UTF8String] ?: "cannot start screen capture");
        return NULL;
    }
}

int bk_screen_take_geometry(void *handle, void **lease, void **data, void **pixel, int *size,
                            int *w, int *h, int *sw, int *sh, int *content_rect, long long *age_ns,
                            char *error, int error_size) {
    @autoreleasepool {
        BKScreen *s = handle;
        [s->lock lock];
        if (s->failure) {
            snprintf(error, error_size, "%s", [s->failure UTF8String]);
            int result = s->rangeRejected ? -2 : -1;
            [s->lock unlock];
            return result;
        }
        if (![s->frames count]) {
            [s->lock unlock];
            return 0;
        }
        NSDictionary *entry = [s->frames[0] retain];
        [s->frames removeObjectAtIndex:0];
        *lease = entry;
        if (content_rect) {
            CGRect rect = [entry[@"rect"] rectValue];
            content_rect[0] = (int)rect.origin.x;
            content_rect[1] = (int)rect.origin.y;
            content_rect[2] = (int)rect.size.width;
            content_rect[3] = (int)rect.size.height;
        }
        *age_ns = CMTimeConvertScale(CMClockGetTime(CMClockGetHostTimeClock()), NSEC_PER_SEC,
                                     kCMTimeRoundingMethod_Default)
                      .value -
                  [entry[@"pts"] longLongValue];
        BKScreenPixel *buffer = entry[@"pixel"];
        *pixel = buffer ? (void *)buffer->pixel : NULL;
        NSData *frame = entry[@"data"];
        *data = frame ? (void *)[frame bytes] : NULL;
        int tag;
        *size =
            buffer ? (int)bk_pixel_layout(buffer->pixel, s->codec == 3, &tag) : (int)[frame length];
        *w = s->width;
        *h = s->height;
        *sw = s->sourceWidth;
        *sh = s->sourceHeight;
        [s->lock unlock];
        return 1;
    }
}
void bk_screen_release(void *lease) {
    [(id)lease release];
}

void bk_screen_lease_timing(void *lease, long long *present, long long *display, long long *acquired,
                           long long *submitted, long long *completed, long long *now, int *repeated) {
    NSDictionary *entry = lease;
    bk_screen_frame_context timing = {0};
    NSValue *value = entry[@"timing"];
    if (value)
        [value getValue:&timing size:sizeof(timing)];
    // Never fall back to the encoder/playback PTS, which may have been advanced
    // for decoder ordering or synthesized for an idle repeat.
    *present = timing.present;
    *display = timing.display;
    *acquired = timing.acquired;
    *submitted = timing.submitted;
    *completed = timing.completed;
    *repeated = timing.repeated;
    *now = bk_screen_host_ns();
}

void bk_screen_lease_texture_timing(void *lease, long long *ready_start,
                                  long long *ready_end, long long *imported) {
    NSDictionary *entry = lease;
    bk_screen_frame_context timing = {0};
    NSValue *value = entry[@"timing"];
    if (value)
        [value getValue:&timing size:sizeof(timing)];
    *ready_start = timing.readyStart;
    *ready_end = timing.readyEnd;
    *imported = timing.imported;
}
int bk_screen_copy_pixel(void *pixel, int nv12, uint8_t *dest, size_t size) {
    @autoreleasepool {
        int tag;
        bk_pixel_layout(pixel, nv12, &tag);
        return bk_pixel_copy(pixel, tag, dest, size);
    }
}
// Compatibility API for native diagnostics. Production borrows the lease and
// copies directly into its final Go destination, or encodes its pixel buffer.
int bk_screen_read_geometry(void *handle, void **data, int *size, int *w, int *h, int *sw, int *sh,
                            int *content_rect, long long *age_ns, char *error, int error_size) {
    void *lease = NULL, *bytes = NULL, *pixel = NULL;
    int result = bk_screen_take_geometry(handle, &lease, &bytes, &pixel, size, w, h, sw, sh,
                                         content_rect, age_ns, error, error_size);
    if (result <= 0)
        return result;
    *data = malloc(*size);
    int ok = *data != NULL;
    if (ok && pixel)
        ok = bk_screen_copy_pixel(pixel, ((BKScreen *)handle)->codec == 3, *data, *size);
    else if (ok)
        memcpy(*data, bytes, *size);
    bk_screen_release(lease);
    if (!ok) {
        free(*data);
        *data = NULL;
        snprintf(error, error_size, "frame copy failed");
        return -1;
    }
    return 1;
}
int bk_screen_read(void *handle, void **data, int *size, int *w, int *h, int *sw, int *sh,
                   long long *age_ns, char *error, int error_size) {
    return bk_screen_read_geometry(handle, data, size, w, h, sw, sh, NULL, age_ns, error,
                                   error_size);
}
void bk_screen_close(void *handle) {
    @autoreleasepool {
        BKScreen *s = handle;
        [s shutdown];
        [s release];
    }
}

// Queried only when Go has received no frame for several seconds. A static
// screen with no outstanding encode is normal; pending encodes remain visible.
int bk_screen_idle_status(void *handle, char *detail, int size) {
    @autoreleasepool {
        BKScreen *s = handle;
        [s->lock lock];
        int expected =
            s->completeFrames > 0 && s->inFlight == 0 && !s->pendingSample &&
            !s->encodeWorkerScheduled && !s->failure &&
            (s->lastStatus == SCFrameStatusComplete || s->lastStatus == SCFrameStatusIdle);
        snprintf(
            detail, size,
            "ScreenCaptureKit callbacks=%llu complete=%llu submitted=%llu encoded=%llu pending=%d queued=%lu busy_drops=%llu queue_resets=%llu idle=%llu repeated=%llu status=%d rdp_full_range=%d range_verified=%d pending_input=%d replaced_input=%llu not_ready_drops=%llu",
            s->callbacks, s->completeFrames, s->submitted, s->encoded, s->inFlight,
            (unsigned long)s->frames.count, s->busyDrops, s->queueResets, s->idleFrames,
            s->repeatedFrames, s->lastStatus, s->rdpFullRange, s->rangeVerified,
            s->pendingSample != NULL, s->pendingReplaced, s->notReadyDrops);
        if (s->timingEnabled && s->encodeCallCount) {
            size_t used = strlen(detail);
            if (used < (size_t)size)
                snprintf(detail + used, size - used, " encode_api_ms=%.3f/%.3f(mean/max)",
                         (double)s->encodeCallTotalNS / s->encodeCallCount / NSEC_PER_MSEC,
                         (double)s->encodeCallMaxNS / NSEC_PER_MSEC);
        }
        [s->lock unlock];
        return expected;
    }
}

int bk_screen_request_keyframe(void *handle) {
    BKScreen *s = handle;
    [s->lock lock];
    if (s->closed || !s->encoder || s->failure) {
        [s->lock unlock];
        return 0;
    }
    BOOL alreadyRequested = s->forceIDR;
    s->forceIDR = YES;
    [s->lock unlock];
    if (!alreadyRequested)
        dispatch_async(s->encodeQueue, ^{
          @autoreleasepool {
              // A refresh must work even if the desktop remains completely unchanged.
              [s->lock lock];
              BOOL active = s->sourceAvailable && !s->pendingSample && !s->closed && !s->failure;
              [s->lock unlock];
              if (s->lastPixel && active)
                  [s encodePixel:s->lastPixel
                          atTime:CMClockGetTime(CMClockGetHostTimeClock())
                          repeat:YES];
          }
        });
    return 1;
}
