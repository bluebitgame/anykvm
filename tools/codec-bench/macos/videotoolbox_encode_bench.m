#import <Foundation/Foundation.h>
#import <VideoToolbox/VideoToolbox.h>
#include <pthread.h>
#include <time.h>
#include <unistd.h>

// Synthetic H.264/HEVC encoder benchmark; never captures the screen.
// Usage: videotoolbox_encode_bench width height fps frames lowLatency maxFrameDelay profile pattern [h264|hevc] [nv12|bgra|i420|bgra_nv12]
// profile: constrained_baseline|baseline|main|high; pattern: flat|moving; maxFrameDelay: -1 leaves default.
// ANYKVM_VT_REFERENCE_BUFFERS optionally sets ReferenceBufferCount (macOS 13+).
typedef struct {
    pthread_mutex_t lock;
    double *submitted, *latency;
    bool *duringDrain;
    int count, callbacks, dropped, errors, draining, drained, spsProfile, spsConstraints;
    int keyframes, predictive, maxPending, maxSteadyPending;
    size_t bytes;
    bool hevc;
    FILE *bitstream;
    FILE *framed;
} Stats;
static double now(void) {
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    return t.tv_sec + t.tv_nsec / 1e9;
}
static void output(void *ref, void *frameRef, OSStatus status, VTEncodeInfoFlags flags,
                   CMSampleBufferRef sample) {
    Stats *s = ref;
    int i = (int)(intptr_t)frameRef - 1;
    double t = now();
    pthread_mutex_lock(&s->lock);
    if (status)
        s->errors++;
    else if ((flags & kVTEncodeInfo_FrameDropped) || !sample)
        s->dropped++;
    else {
        s->callbacks++;
        CFArrayRef attachments = CMSampleBufferGetSampleAttachmentsArray(sample, false);
        BOOL key = !attachments || !CFArrayGetCount(attachments) ||
                   !CFDictionaryContainsKey(CFArrayGetValueAtIndex(attachments, 0),
                                            kCMSampleAttachmentKey_NotSync);
        if (key)
            s->keyframes++;
        else
            s->predictive++;
        CMBlockBufferRef payload = CMSampleBufferGetDataBuffer(sample);
        if (payload)
            s->bytes += CMBlockBufferGetDataLength(payload);
        if (s->draining)
            s->drained++;
        if (i >= 0 && i < s->count) {
            s->latency[i] = (t - s->submitted[i]) * 1000;
            s->duringDrain[i] = s->draining;
        }
        if (s->bitstream) {
            NSMutableData *au = [NSMutableData data];
            const uint8_t start[] = {0, 0, 0, 1};
            CMFormatDescriptionRef desc = CMSampleBufferGetFormatDescription(sample);
            size_t count = 0;
            int lengthBytes = 0;
            OSStatus err = s->hevc ? CMVideoFormatDescriptionGetHEVCParameterSetAtIndex(
                                         desc, 0, NULL, NULL, &count, &lengthBytes)
                                   : CMVideoFormatDescriptionGetH264ParameterSetAtIndex(
                                         desc, 0, NULL, NULL, &count, &lengthBytes);
            if (key && !err)
                for (size_t j = 0; j < count; j++) {
                    const uint8_t *p = NULL;
                    size_t size = 0;
                    err = s->hevc ? CMVideoFormatDescriptionGetHEVCParameterSetAtIndex(
                                        desc, j, &p, &size, NULL, NULL)
                                  : CMVideoFormatDescriptionGetH264ParameterSetAtIndex(
                                        desc, j, &p, &size, NULL, NULL);
                    if (err)
                        break;
                    [au appendBytes:start length:4];
                    [au appendBytes:p length:size];
                }
            CMBlockBufferRef block = CMSampleBufferGetDataBuffer(sample);
            size_t size = CMBlockBufferGetDataLength(block);
            NSMutableData *data = [NSMutableData dataWithLength:size];
            if (CMBlockBufferCopyDataBytes(block, 0, size, data.mutableBytes))
                err = -1;
            const uint8_t *p = data.bytes;
            size_t offset = 0;
            if (lengthBytes < 1 || lengthBytes > 4)
                err = -1;
            while (!err && offset < size) {
                if (size - offset < (size_t)lengthBytes) {
                    err = -1;
                    break;
                }
                uint32_t len = 0;
                for (int j = 0; j < lengthBytes; j++)
                    len = (len << 8) | p[offset++];
                if (!len || len > size - offset) {
                    err = -1;
                    break;
                }
                [au appendBytes:start length:4];
                [au appendBytes:p + offset length:len];
                offset += len;
            }
            if (err)
                s->errors++;
            else {
                fwrite(au.bytes, 1, au.length, s->bitstream);
                if (s->framed) {
                    uint32_t len = CFSwapInt32HostToBig((uint32_t)au.length);
                    fwrite(&len, 4, 1, s->framed);
                    fwrite(au.bytes, 1, au.length, s->framed);
                }
            }
        }
        const uint8_t *sps = NULL;
        size_t size = 0;
        if (!CMVideoFormatDescriptionGetH264ParameterSetAtIndex(
                CMSampleBufferGetFormatDescription(sample), 0, &sps, &size, NULL, NULL) &&
            size > 2) {
            s->spsProfile = sps[1];
            s->spsConstraints = sps[2];
        }
    }
    pthread_mutex_unlock(&s->lock);
}
static void set(VTCompressionSessionRef session, CFStringRef key, CFTypeRef value) {
    OSStatus status = VTSessionSetProperty(session, key, value);
    if (status) {
        fprintf(stderr, "property %s failed: %d\n", [(__bridge NSString *)key UTF8String],
                (int)status);
        exit(2);
    }
}
static id get(VTCompressionSessionRef session, CFStringRef key) {
    CFTypeRef value = NULL;
    OSStatus status = VTSessionCopyProperty(session, key, NULL, &value);
    return status || !value ? (id)[NSNull null] : CFBridgingRelease(value);
}
static int compare(const void *a, const void *b) {
    double x = *(const double *)a, y = *(const double *)b;
    return (x > y) - (x < y);
}
int main(int argc, char **argv) {
    @autoreleasepool {
        if (argc != 9 && argc != 11) {
            fprintf(
                stderr,
                "width height fps frames lowLatency maxFrameDelay constrained_baseline|baseline|main|high flat|moving\n");
            return 2;
        }
        int w = atoi(argv[1]), h = atoi(argv[2]), fps = atoi(argv[3]), n = atoi(argv[4]);
        BOOL low = atoi(argv[5]), moving = !strcmp(argv[8], "moving");
        CFStringRef h264Profile = kVTProfileLevel_H264_Baseline_AutoLevel;
        if (!strcmp(argv[7], "constrained_baseline"))
            h264Profile = kVTProfileLevel_H264_ConstrainedBaseline_AutoLevel;
        else if (!strcmp(argv[7], "main"))
            h264Profile = kVTProfileLevel_H264_Main_AutoLevel;
        else if (!strcmp(argv[7], "high"))
            h264Profile = kVTProfileLevel_H264_High_AutoLevel;
        else if (strcmp(argv[7], "baseline")) {
            fprintf(stderr, "invalid H.264 profile\n");
            return 2;
        }
        int delay = atoi(argv[6]);
        BOOL hevc = argc == 11 && !strcmp(argv[9], "hevc");
        const char *pixelName = argc == 11 ? argv[10] : "nv12";
        BOOL convert = !strcmp(pixelName, "bgra_nv12");
        BOOL bgra = !strcmp(pixelName, "bgra") || convert;
        BOOL planar = !strcmp(pixelName, "i420");
        OSType pixelType = bgra ? kCVPixelFormatType_32BGRA
                                : (planar ? kCVPixelFormatType_420YpCbCr8Planar
                                          : kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange);
        double *conversion = calloc(n, sizeof(double));
        VTPixelTransferSessionRef transfer = NULL;
        if (convert && VTPixelTransferSessionCreate(NULL, &transfer))
            return 2;
        if (w < 16 || h < 16 || w > 8192 || h > 8192 || (w % 2) || (h % 2) || fps < 1 ||
            fps > 240 || n < 90 || n > 10000)
            return 2;
        Stats s = {.lock = PTHREAD_MUTEX_INITIALIZER, .count = n, .hevc = hevc};
        const char *outPath = getenv("ANYKVM_VT_BITSTREAM"),
                   *framesPath = getenv("ANYKVM_VT_FRAMES");
        if (outPath && !(s.bitstream = fopen(outPath, "wb")))
            return 2;
        if (framesPath && !(s.framed = fopen(framesPath, "wb")))
            return 2;
        s.submitted = calloc(n, sizeof(double));
        s.latency = calloc(n, sizeof(double));
        s.duringDrain = calloc(n, sizeof(bool));
        NSMutableDictionary *spec = [@{
            (__bridge NSString *)
            kVTVideoEncoderSpecification_RequireHardwareAcceleratedVideoEncoder : @YES
        } mutableCopy];
        if (low)
            spec[(__bridge NSString *)kVTVideoEncoderSpecification_EnableLowLatencyRateControl] =
                @YES;
        NSDictionary *attrs = @{
            (__bridge NSString *)kCVPixelBufferPixelFormatTypeKey :
                @(convert ? kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange : pixelType),
            (__bridge NSString *)kCVPixelBufferWidthKey : @(w),
            (__bridge NSString *)kCVPixelBufferHeightKey : @(h),
            (__bridge NSString *)kCVPixelBufferIOSurfacePropertiesKey : @{}
        };
        VTCompressionSessionRef session = NULL;
        OSStatus status = VTCompressionSessionCreate(
            NULL, w, h, hevc ? kCMVideoCodecType_HEVC : kCMVideoCodecType_H264,
            (__bridge CFDictionaryRef)spec, (__bridge CFDictionaryRef)attrs, NULL, output, &s,
            &session);
        if (status) {
            fprintf(stderr, "create failed: %d\n", (int)status);
            return 2;
        }
        BOOL speed = getenv("ANYKVM_VT_SPEED") && atoi(getenv("ANYKVM_VT_SPEED"));
        set(session, kVTCompressionPropertyKey_RealTime, kCFBooleanTrue);
        set(session, kVTCompressionPropertyKey_AllowFrameReordering, kCFBooleanFalse);
        set(session, kVTCompressionPropertyKey_ProfileLevel,
            hevc ? kVTProfileLevel_HEVC_Main_AutoLevel : h264Profile);
        set(session, kVTCompressionPropertyKey_AverageBitRate,
            (__bridge CFNumberRef) @(w > 640 ? 12000000 : 4000000));
        set(session, kVTCompressionPropertyKey_ExpectedFrameRate, (__bridge CFNumberRef) @(fps));
        set(session, kVTCompressionPropertyKey_MaxKeyFrameInterval, (__bridge CFNumberRef) @(fps));
        if (low && !hevc) {
            // Match the production GameStream compatibility settings.
            set(session, kVTCompressionPropertyKey_BaseLayerFrameRateFraction,
                (__bridge CFNumberRef) @1.0);
            if (@available(macOS 12.0, *))
                set(session, kVTCompressionPropertyKey_EnableLTR, kCFBooleanFalse);
        }
        const char *referenceEnv = getenv("ANYKVM_VT_REFERENCE_BUFFERS");
        int references = referenceEnv ? atoi(referenceEnv) : -1;
        if (referenceEnv) {
            if (references < 1) {
                fprintf(stderr, "reference buffer count must be positive\n");
                return 2;
            }
            if (@available(macOS 13.0, *))
                set(session, kVTCompressionPropertyKey_ReferenceBufferCount,
                    (__bridge CFNumberRef) @(references));
            else {
                fprintf(stderr, "reference buffer count requires macOS 13 or later\n");
                return 2;
            }
        }
        if (delay >= 0)
            set(session, kVTCompressionPropertyKey_MaxFrameDelayCount,
                (__bridge CFNumberRef) @(delay));
        if (speed)
            set(session, kVTCompressionPropertyKey_PrioritizeEncodingSpeedOverQuality,
                kCFBooleanTrue);
        status = VTCompressionSessionPrepareToEncodeFrames(session);
        if (status) {
            fprintf(stderr, "prepare failed: %d\n", (int)status);
            return 2;
        }
        NSMutableDictionary *result = [@{
            @"speed_priority" : @(speed),
            @"codec" : hevc ? @"hevc" : @"h264",
            @"pixel_format" : @(pixelName),
            @"width" : @(w),
            @"height" : @(h),
            @"fps" : @(fps),
            @"frames" : @(n),
            @"low_latency_requested" : @(low),
            @"max_frame_delay_requested" : @(delay),
            @"profile_requested" : @(argv[7]),
            @"pattern" : @(argv[8]),
            @"encoder" : get(session, kVTCompressionPropertyKey_EncoderID),
            @"hardware" :
                get(session, kVTCompressionPropertyKey_UsingHardwareAcceleratedVideoEncoder),
            @"profile_actual" : get(session, kVTCompressionPropertyKey_ProfileLevel),
            @"max_frame_delay_actual" : get(session, kVTCompressionPropertyKey_MaxFrameDelayCount)
        } mutableCopy];
        result[@"reference_buffer_count_requested"] = @(references);
        result[@"speed_priority_actual"] =
            get(session, kVTCompressionPropertyKey_PrioritizeEncodingSpeedOverQuality);
        CFDictionaryRef supportedProperties = NULL;
        OSStatus supportedStatus =
            VTSessionCopySupportedPropertyDictionary(session, &supportedProperties);
        result[@"max_frame_delay_supported_dictionary_status"] = @(supportedStatus);
        result[@"max_frame_delay_listed_as_supported"] =
            @(supportedProperties &&
              CFDictionaryContainsKey(supportedProperties,
                                      kVTCompressionPropertyKey_MaxFrameDelayCount));
        if (supportedProperties)
            result[@"max_frame_delay_property_info"] =
                (__bridge id)CFDictionaryGetValue(supportedProperties,
                                                  kVTCompressionPropertyKey_MaxFrameDelayCount)
                    ?: (id)[NSNull null];
        if (supportedProperties)
            CFRelease(supportedProperties);
        if (@available(macOS 13.0, *))
            result[@"reference_buffer_count_actual"] =
                get(session, kVTCompressionPropertyKey_ReferenceBufferCount);
        double previous = 0;
        for (int i = 0; i < n; i++) {
            double wait = previous + 1.0 / fps - now();
            if (wait > 0)
                usleep((useconds_t)(wait * 1e6));
            previous = now();
            CVPixelBufferRef pixel = NULL;
            if (convert) {
                NSDictionary *inputAttrs =
                    @{(__bridge NSString *)kCVPixelBufferIOSurfacePropertiesKey : @{}};
                status = CVPixelBufferCreate(NULL, w, h, pixelType,
                                             (__bridge CFDictionaryRef)inputAttrs, &pixel);
            } else
                status = CVPixelBufferPoolCreatePixelBuffer(
                    NULL, VTCompressionSessionGetPixelBufferPool(session), &pixel);
            if (status) {
                fprintf(stderr, "pixel allocation failed: %d\n", (int)status);
                return 2;
            }
            CVPixelBufferLockBaseAddress(pixel, 0);
            if (bgra) {
                size_t stride = CVPixelBufferGetBytesPerRow(pixel);
                uint8_t *p = CVPixelBufferGetBaseAddress(pixel);
                for (int row = 0; row < h; row++)
                    for (int col = 0; col < w; col++) {
                        int luma = moving ? 32 + (((col + i * 8) / 32 + row / 32) % 2) * 160
                                          : 32 + i % 160;
                        uint8_t level = (luma - 16) * 255 / 219;
                        size_t offset = row * stride + col * 4;
                        p[offset] = p[offset + 1] = p[offset + 2] = level;
                        p[offset + 3] = 255;
                    }
            } else {
                size_t stride = CVPixelBufferGetBytesPerRowOfPlane(pixel, 0);
                uint8_t *y = CVPixelBufferGetBaseAddressOfPlane(pixel, 0);
                for (int row = 0; row < h; row++) {
                    if (!moving)
                        memset(y + row * stride, 32 + i % 160, w);
                    else
                        for (int col = 0; col < w; col++)
                            y[row * stride + col] =
                                32 + (((col + i * 8) / 32 + row / 32) % 2) * 160;
                }
                for (size_t plane = 1; plane < CVPixelBufferGetPlaneCount(pixel); plane++) {
                    uint8_t *uv = CVPixelBufferGetBaseAddressOfPlane(pixel, plane);
                    size_t strideUV = CVPixelBufferGetBytesPerRowOfPlane(pixel, plane);
                    for (int row = 0; row < h / 2; row++)
                        memset(uv + row * strideUV, 128, planar ? w / 2 : w);
                }
            }
            CVPixelBufferUnlockBaseAddress(pixel, 0);
            if (convert) {
                CVPixelBufferRef nv12 = NULL;
                status = CVPixelBufferPoolCreatePixelBuffer(
                    NULL, VTCompressionSessionGetPixelBufferPool(session), &nv12);
                if (status)
                    return 2;
                double begin = now();
                status = VTPixelTransferSessionTransferImage(transfer, pixel, nv12);
                conversion[i] = (now() - begin) * 1000;
                CVPixelBufferRelease(pixel);
                pixel = nv12;
                if (status) {
                    fprintf(stderr, "transfer failed: %d\n", (int)status);
                    return 2;
                }
            }
            // Frame preparation is excluded; measure only submission-to-callback.
            pthread_mutex_lock(&s.lock);
            s.submitted[i] = now();
            pthread_mutex_unlock(&s.lock);
            status = VTCompressionSessionEncodeFrame(session, pixel, CMTimeMake(i, fps),
                                                     CMTimeMake(1, fps), NULL,
                                                     (void *)(intptr_t)(i + 1), NULL);
            pthread_mutex_lock(&s.lock);
            int pending = i + 1 - s.callbacks - s.dropped - s.errors;
            if (pending > s.maxPending)
                s.maxPending = pending;
            if (i >= 30 && i < n - 15 && pending > s.maxSteadyPending)
                s.maxSteadyPending = pending;
            pthread_mutex_unlock(&s.lock);
            CVPixelBufferRelease(pixel);
            if (status) {
                fprintf(stderr, "encode failed at %d: %d\n", i, (int)status);
                return 2;
            }
        }
        pthread_mutex_lock(&s.lock);
        s.draining = 1;
        pthread_mutex_unlock(&s.lock);
        status = VTCompressionSessionCompleteFrames(session, kCMTimeInvalid);
        if (status) {
            fprintf(stderr, "complete failed: %d\n", (int)status);
            return 2;
        }
        // Exclude warmup and the tail affected by final draining.
        double *values = calloc(n, sizeof(double));
        int count = 0;
        for (int i = 30; i < n - 15; i++)
            if (s.latency[i] > 0 && !s.duringDrain[i])
                values[count++] = s.latency[i];
        qsort(values, count, sizeof(double), compare);
        result[@"samples"] = @(count);
        result[@"callbacks"] = @(s.callbacks);
        result[@"dropped"] = @(s.dropped);
        result[@"errors"] = @(s.errors);
        result[@"callbacks_during_final_drain"] = @(s.drained);
        result[@"sps_profile_idc"] = @(s.spsProfile);
        result[@"sps_constraints"] = @(s.spsConstraints);
        result[@"keyframes"] = @(s.keyframes);
        result[@"predictive_frames"] = @(s.predictive);
        result[@"encoded_bytes"] = @(s.bytes);
        result[@"max_pending_at_encode_return"] = @(s.maxPending);
        result[@"max_steady_pending_at_encode_return"] = @(s.maxSteadyPending);
        if (count) {
            result[@"median_ms"] = @(values[count / 2]);
            result[@"p95_ms"] = @(values[(count - 1) * 95 / 100]);
        }
        double *total = calloc(n, sizeof(double)), *conversions = calloc(n, sizeof(double));
        int totals = 0;
        for (int i = 30; i < n - 15; i++)
            if (s.latency[i] > 0 && !s.duringDrain[i]) {
                total[totals] = s.latency[i] + conversion[i];
                conversions[totals++] = conversion[i];
            }
        qsort(total, totals, sizeof(double), compare);
        qsort(conversions, totals, sizeof(double), compare);
        if (totals) {
            result[@"with_conversion_median_ms"] = @(total[totals / 2]);
            result[@"with_conversion_p95_ms"] = @(total[(totals - 1) * 95 / 100]);
            result[@"conversion_median_ms"] = @(conversions[totals / 2]);
            result[@"conversion_p95_ms"] = @(conversions[(totals - 1) * 95 / 100]);
        }
        free(total);
        free(conversions);
        free(conversion);
        if (transfer) {
            VTPixelTransferSessionInvalidate(transfer);
            CFRelease(transfer);
        }
        result[@"first_frame_ms"] = @(s.latency[0]);
        CFTypeRef delayValue = NULL;
        OSStatus delayReadbackStatus = VTSessionCopyProperty(
            session, kVTCompressionPropertyKey_MaxFrameDelayCount, NULL, &delayValue);
        result[@"max_frame_delay_readback_status"] = @(delayReadbackStatus);
        result[@"max_frame_delay_actual_after_encode"] =
            delayValue ? CFBridgingRelease(delayValue) : (id)[NSNull null];
        NSData *json = [NSJSONSerialization dataWithJSONObject:result
                                                       options:NSJSONWritingSortedKeys
                                                         error:nil];
        puts([[NSString alloc] initWithData:json encoding:NSUTF8StringEncoding].UTF8String);
        VTCompressionSessionInvalidate(session);
        CFRelease(session);
        if (s.bitstream)
            fclose(s.bitstream);
        if (s.framed)
            fclose(s.framed);
        free(values);
        free(s.submitted);
        free(s.latency);
        free(s.duringDrain);
        pthread_mutex_destroy(&s.lock);
        return s.errors || s.dropped || s.callbacks != n ? 1 : 0;
    }
}
