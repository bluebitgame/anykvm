//go:build darwin && cgo

#import <Foundation/Foundation.h>
#import <VideoToolbox/VideoToolbox.h>
#include "native.h"
#include "reference_darwin.h"
#include "annexb_darwin.h"
#include "pixeltransfer_darwin.h"

struct bk_encoder {
    VTCompressionSessionRef session;
    VTPixelTransferSessionRef transfer;
    int w, h, fps_num, fps_den, hevc, status, inFlight, limit;
    int64_t frame;
    NSLock *lock;
    NSMutableArray *output;
    NSMutableDictionary *pixels; // explicit retains, keyed by submission ID
    dispatch_queue_t worker;
    BOOL closed, busy;
};

static NSData *annexb(bk_encoder *e, CMSampleBufferRef sample, OSStatus *status) {
    CFArrayRef attachments = CMSampleBufferGetSampleAttachmentsArray(sample, false);
    BOOL key = !attachments || !CFArrayGetCount(attachments) ||
               !CFDictionaryContainsKey(CFArrayGetValueAtIndex(attachments, 0),
                                        kCMSampleAttachmentKey_NotSync);
    CMFormatDescriptionRef format = CMSampleBufferGetFormatDescription(sample);
    int nalLength = 4;
    size_t count = 0, size = 0;
    const uint8_t *bytes = NULL;
    if (e->hevc)
        *status = CMVideoFormatDescriptionGetHEVCParameterSetAtIndex(format, 0, &bytes, &size,
                                                                     &count, &nalLength);
    else
        *status = CMVideoFormatDescriptionGetH264ParameterSetAtIndex(format, 0, &bytes, &size,
                                                                     &count, &nalLength);
    if (*status || nalLength < 1 || nalLength > 4) {
        if (!*status)
            *status = -2;
        return nil;
    }
    bk_parameter_set parameters[64];
    if (count > 64) {
        *status = -2;
        return nil;
    }
    if (key)
        for (size_t i = 0; i < count; i++) {
            if (e->hevc)
                *status = CMVideoFormatDescriptionGetHEVCParameterSetAtIndex(format, i, &bytes,
                                                                             &size, NULL, NULL);
            else
                *status = CMVideoFormatDescriptionGetH264ParameterSetAtIndex(format, i, &bytes,
                                                                             &size, NULL, NULL);
            if (*status)
                return nil;
            parameters[i] = (bk_parameter_set){bytes, size};
        }
    CMBlockBufferRef block = CMSampleBufferGetDataBuffer(sample);
    return bk_annexb_block(block, nalLength, parameters, key ? count : 0, status);
}

static void encoded(void *opaque, void *source, OSStatus status, VTEncodeInfoFlags flags,
                    CMSampleBufferRef sample) {
    @autoreleasepool {
        bk_encoder *e = opaque;
        NSNumber *id = @((uint64_t)(uintptr_t)source);
        BOOL dropped = !sample || (flags & kVTEncodeInfo_FrameDropped);
        NSData *data = !status && !dropped ? annexb(e, sample, &status) : nil;
        [e->lock lock];
        NSValue *value = e->pixels[id];
        if (value) {
            CVPixelBufferRelease([value pointerValue]);
            [e->pixels removeObjectForKey:id];
        }
        e->inFlight--;
        if (!e->closed) {
            if (status)
                e->status = status;
            else
                [e->output addObject:@{
                    @"id" : id,
                    @"data" : data ?: [NSData data],
                    @"dropped" : @(dropped)
                }];
        }
        [e->lock unlock];
    }
}

bk_encoder *bk_encoder_open(const char *backend, const char *device, int w, int h, int fps_num,
                            int fps_den, int bitrate, int hevc, int disable_low_latency,
                            int h264_profile, char *error, size_t error_size) {
    @autoreleasepool {
        (void)backend;
        (void)device;
        bk_encoder *e = calloc(1, sizeof(*e));
        if (!e)
            return NULL;
        e->w = w;
        e->h = h;
        e->fps_num = fps_num;
        e->fps_den = fps_den;
        e->hevc = hevc;
        // HEVC lookahead can add two frame periods even with reordering disabled.
        // Flush on a background worker and admit one picture to avoid that backlog.
        e->limit = hevc ? 1 : 2;
        if (hevc)
            e->worker = dispatch_queue_create("anykvm.hevc", DISPATCH_QUEUE_SERIAL);
        e->lock = [NSLock new];
        e->output = [NSMutableArray new];
        e->pixels = [NSMutableDictionary new];
        // Default to Constrained Baseline; allow alternate H.264 profiles for decoder compatibility.
        // Keep frame reordering disabled independently of the selected profile.
        CFStringRef h264Profile = kVTProfileLevel_H264_ConstrainedBaseline_AutoLevel;
        switch (h264_profile) {
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
        if (!hevc && !disable_low_latency)
            spec[(id)kVTVideoEncoderSpecification_EnableLowLatencyRateControl] = @YES;
        NSDictionary *attrs = @{
            (id)
            kCVPixelBufferPixelFormatTypeKey : @(kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange),
            (id)kCVPixelBufferIOSurfacePropertiesKey : @{}
        };
        OSStatus status = VTCompressionSessionCreate(
            NULL, w, h, hevc ? kCMVideoCodecType_HEVC : kCMVideoCodecType_H264,
            (CFDictionaryRef)spec, (CFDictionaryRef)attrs, NULL, encoded, e, &e->session);
        if (!status) {
            NSMutableDictionary *properties = [@{
                (id)kVTCompressionPropertyKey_RealTime : @YES,
                (id)kVTCompressionPropertyKey_AllowFrameReordering : @NO,
                (id)kVTCompressionPropertyKey_ProfileLevel :
                        hevc ? (id)kVTProfileLevel_HEVC_Main_AutoLevel : (id)h264Profile,
                (id)kVTCompressionPropertyKey_ExpectedFrameRate : @((double)fps_num / fps_den),
                (id)kVTCompressionPropertyKey_MaxKeyFrameInterval :
                    @(((int64_t)fps_num + fps_den - 1) / fps_den)
            } mutableCopy];
            if (bitrate > 0)
                properties[(id)kVTCompressionPropertyKey_AverageBitRate] = @(bitrate * 1000);
            status = VTSessionSetProperties(e->session, (CFDictionaryRef)properties);
        }
        if (!status && !hevc && !disable_low_latency) {
            // Match the direct screen encoder: a single temporal layer and no VT LTR
            // acknowledgement protocol, while retaining low-latency rate control.
            status = VTSessionSetProperty(e->session,
                                          kVTCompressionPropertyKey_BaseLayerFrameRateFraction,
                                          (CFNumberRef) @1.0);
            if (!status) {
                if (@available(macOS 12.0, *))
                    status = VTSessionSetProperty(e->session, kVTCompressionPropertyKey_EnableLTR,
                                                  kCFBooleanFalse);
            }
        }
        if (!status && !hevc)
            status = bk_h264_reference_buffers(e->session);
        if (!status)
            status = VTCompressionSessionPrepareToEncodeFrames(e->session);
        if (status) {
            snprintf(error, error_size, "VideoToolbox initialization failed (%d)", (int)status);
            bk_encoder_close(e);
            return NULL;
        }
        return e;
    }
}

static int can_submit(bk_encoder *e, char *error, size_t error_size) {
    [e->lock lock];
    int result =
        e->closed || e->status ? -1 : (!e->busy && e->inFlight + [e->output count] < e->limit);
    if (result < 0)
        snprintf(error, error_size, "VideoToolbox session failed (%d)", e->status);
    [e->lock unlock];
    return result;
}
int bk_encoder_ready(bk_encoder *e, char *error, size_t error_size) {
    return can_submit(e, error, error_size);
}

int bk_encoder_submit_pixel(bk_encoder *e, void *buffer, uint64_t frame_id, int idr, char *error,
                            size_t error_size) {
    @autoreleasepool {
        int ready = can_submit(e, error, error_size);
        if (ready <= 0)
            return ready;
        CVPixelBufferRef pixel = buffer, converted = NULL;
        if (!pixel || !frame_id) {
            snprintf(error, error_size, "invalid pixel buffer or submission ID");
            return -1;
        }
        OSStatus status = 0;
        if (CVPixelBufferGetWidth(pixel) != (size_t)e->w ||
            CVPixelBufferGetHeight(pixel) != (size_t)e->h ||
            CVPixelBufferGetPixelFormatType(pixel) !=
                kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange) {
            if (!e->transfer) {
                status = VTPixelTransferSessionCreate(NULL, &e->transfer);
                if (!status)
                    status =
                        VTSessionSetProperty(e->transfer, kVTPixelTransferPropertyKey_ScalingMode,
                                             kVTScalingMode_Letterbox);
            }
            if (!status)
                status = CVPixelBufferPoolCreatePixelBuffer(
                    NULL, VTCompressionSessionGetPixelBufferPool(e->session), &converted);
            if (!status)
                status = bk_transfer_nv12(e->transfer, pixel, converted);
            pixel = converted;
        }
        if (status) {
            if (converted)
                CVPixelBufferRelease(converted);
            snprintf(error, error_size, "VideoToolbox pixel transfer failed (%d)", (int)status);
            return -1;
        }
        NSNumber *key = @(frame_id);
        [e->lock lock];
        if (e->pixels[key]) {
            [e->lock unlock];
            if (converted)
                CVPixelBufferRelease(converted);
            snprintf(error, error_size, "duplicate submission ID");
            return -1;
        }
        e->pixels[key] = [NSValue valueWithPointer:CVPixelBufferRetain(pixel)];
        e->inFlight++;
        if (e->hevc)
            e->busy = YES;
        [e->lock unlock];
        NSDictionary *props = idr ? @{(id)kVTEncodeFrameOptionKey_ForceKeyFrame : @YES} : nil;
        CMTime pts = CMTimeMake(e->frame++ * e->fps_den, e->fps_num);
        if (e->hevc) {
            dispatch_async(e->worker, ^{
              @autoreleasepool {
                  OSStatus result = VTCompressionSessionEncodeFrame(
                      e->session, pixel, pts, CMTimeMake(e->fps_den, e->fps_num),
                      (CFDictionaryRef)props, (void *)(uintptr_t)frame_id, NULL);
                  if (!result)
                      result = VTCompressionSessionCompleteFrames(e->session, kCMTimeInvalid);
                  [e->lock lock];
                  if (result) {
                      e->status = result;
                      NSValue *value = e->pixels[key];
                      if (value) {
                          CVPixelBufferRelease([value pointerValue]);
                          [e->pixels removeObjectForKey:key];
                          e->inFlight--;
                      }
                  }
                  e->busy = NO;
                  [e->lock unlock];
              }
            });
        } else {
            status = VTCompressionSessionEncodeFrame(
                e->session, pixel, pts, CMTimeMake(e->fps_den, e->fps_num), (CFDictionaryRef)props,
                (void *)(uintptr_t)frame_id, NULL);
        }
        if (converted)
            CVPixelBufferRelease(converted);
        if (status) {
            [e->lock lock];
            NSValue *value = e->pixels[key];
            if (value) {
                CVPixelBufferRelease([value pointerValue]);
                [e->pixels removeObjectForKey:key];
                e->inFlight--;
            }
            [e->lock unlock];
            snprintf(error, error_size, "VideoToolbox submit failed (%d)", (int)status);
            return -1;
        }
        return 1;
    }
}

int bk_encoder_submit(bk_encoder *e, const uint8_t *data, int w, int h, int format,
                      uint64_t frame_id, int idr, char *error, size_t error_size) {
    @autoreleasepool {
        int ready = can_submit(e, error, error_size);
        if (ready <= 0)
            return ready;
        OSType type = format == 1   ? kCVPixelFormatType_32BGRA
                      : format == 2 ? kCVPixelFormatType_422YpCbCr8
                      : format == 3 ? kCVPixelFormatType_422YpCbCr8_yuvs
                                    : kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange;
        CVPixelBufferRef pixel = NULL;
        NSDictionary *attrs = @{(id)kCVPixelBufferIOSurfacePropertiesKey : @{}};
        CVReturn status = CVPixelBufferCreate(NULL, w, h, type, (CFDictionaryRef)attrs, &pixel);
        if (!status)
            status = CVPixelBufferLockBaseAddress(pixel, 0);
        if (!status) {
            if (format == 4) {
                for (int plane = 0; plane < 2; plane++) {
                    uint8_t *dest = CVPixelBufferGetBaseAddressOfPlane(pixel, plane);
                    size_t pitch = CVPixelBufferGetBytesPerRowOfPlane(pixel, plane);
                    const uint8_t *src = data + (plane ? w * h : 0);
                    for (int y = 0; y < (plane ? h / 2 : h); y++)
                        memcpy(dest + y * pitch, src + y * w, w);
                }
            } else {
                uint8_t *dest = CVPixelBufferGetBaseAddress(pixel);
                size_t pitch = CVPixelBufferGetBytesPerRow(pixel), row = w * (format == 1 ? 4 : 2);
                for (int y = 0; y < h; y++)
                    memcpy(dest + y * pitch, data + y * row, row);
            }
            CVPixelBufferUnlockBaseAddress(pixel, 0);
        }
        int result;
        if (status) {
            snprintf(error, error_size, "VideoToolbox input allocation failed (%d)", status);
            result = -1;
        } else
            result = bk_encoder_submit_pixel(e, pixel, frame_id, idr, error, error_size);
        if (pixel)
            CVPixelBufferRelease(pixel);
        return result;
    }
}

int bk_encoder_receive(bk_encoder *e, void **lease, const uint8_t **data, size_t *size,
                       uint64_t *id, int *dropped, char *error, size_t error_size) {
    @autoreleasepool {
        *lease = NULL;
        *data = NULL;
        *size = 0;
        [e->lock lock];
        if (e->status || e->closed) {
            snprintf(error, error_size, "VideoToolbox callback failed (%d)", e->status);
            [e->lock unlock];
            return -1;
        }
        if (e->busy || ![e->output count]) {
            [e->lock unlock];
            return 0;
        }
        NSDictionary *packet = e->output[0];
        NSData *bytes = [packet[@"data"] retain];
        *lease = bytes;
        *data = [bytes bytes];
        *size = [bytes length];
        *id = [packet[@"id"] unsignedLongLongValue];
        *dropped = [packet[@"dropped"] boolValue];
        [e->output removeObjectAtIndex:0];
        [e->lock unlock];
        return 1;
    }
}
void bk_encoder_packet_release(void *lease) {
    [(id)lease release];
}
int bk_encoder_flush(bk_encoder *e, char *error, size_t error_size) {
    // Compatibility calls wait for the existing worker, never flush concurrently.
    OSStatus status = 0;
    if (e->worker) {
        dispatch_sync(e->worker, ^{
                      });
        [e->lock lock];
        status = e->status;
        [e->lock unlock];
    } else
        status = VTCompressionSessionCompleteFrames(e->session, kCMTimeInvalid);
    if (status)
        snprintf(error, error_size, "VideoToolbox flush failed (%d)", (int)status);
    return status ? -1 : 0;
}
void bk_encoder_close(bk_encoder *e) {
    if (!e)
        return;
    [e->lock lock];
    e->closed = YES;
    [e->lock unlock];
    if (e->worker) {
        dispatch_sync(e->worker, ^{
                      });
        dispatch_release(e->worker);
    }
    // Drain any remaining work on teardown, after the HEVC worker has stopped.
    if (e->session) {
        VTCompressionSessionCompleteFrames(e->session, kCMTimeInvalid);
        VTCompressionSessionInvalidate(e->session);
        CFRelease(e->session);
    }
    if (e->transfer) {
        VTPixelTransferSessionInvalidate(e->transfer);
        CFRelease(e->transfer);
    }
    for (NSValue *value in [e->pixels allValues])
        CVPixelBufferRelease([value pointerValue]);
    [e->pixels release];
    [e->output release];
    [e->lock release];
    free(e);
}
