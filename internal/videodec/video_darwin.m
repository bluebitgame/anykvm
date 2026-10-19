#import <Foundation/Foundation.h>
#import <VideoToolbox/VideoToolbox.h>
#include "video_darwin.h"

struct anykvm_airplay_video {
    VTDecompressionSessionRef session;
    CMVideoFormatDescriptionRef format;
    CVPixelBufferRef image;
    OSStatus status;
};

static void anykvm_airplay_video_output(void *opaque, void *frame, OSStatus status,
                                        VTDecodeInfoFlags flags, CVImageBufferRef image, CMTime pts,
                                        CMTime duration) {
    anykvm_airplay_video *decoder = opaque;
    decoder->status = status;
    if (image && !status) {
        if (decoder->image)
            CVPixelBufferRelease(decoder->image);
        decoder->image = CVPixelBufferRetain(image);
    }
}

static void anykvm_airplay_video_reset(anykvm_airplay_video *decoder) {
    if (decoder->session) {
        VTDecompressionSessionWaitForAsynchronousFrames(decoder->session);
        VTDecompressionSessionInvalidate(decoder->session);
        CFRelease(decoder->session);
        decoder->session = NULL;
    }
    if (decoder->format) {
        CFRelease(decoder->format);
        decoder->format = NULL;
    }
    if (decoder->image) {
        CVPixelBufferRelease(decoder->image);
        decoder->image = NULL;
    }
}

anykvm_airplay_video *anykvm_airplay_video_new(void) {
    return calloc(1, sizeof(anykvm_airplay_video));
}

int anykvm_airplay_video_configure(anykvm_airplay_video *decoder, const uint8_t *sps,
                                   size_t sps_size, const uint8_t *pps, size_t pps_size) {
    anykvm_airplay_video_reset(decoder);
    const uint8_t *sets[] = {sps, pps};
    size_t sizes[] = {sps_size, pps_size};
    OSStatus status = CMVideoFormatDescriptionCreateFromH264ParameterSets(
        kCFAllocatorDefault, 2, sets, sizes, 4, &decoder->format);
    if (status)
        return status;
    VTDecompressionOutputCallbackRecord callback = {anykvm_airplay_video_output, decoder};
    @autoreleasepool {
        NSDictionary *attributes =
            @{(id)kCVPixelBufferPixelFormatTypeKey : @(kCVPixelFormatType_32BGRA)};
        status =
            VTDecompressionSessionCreate(kCFAllocatorDefault, decoder->format, NULL,
                                         (CFDictionaryRef)attributes, &callback, &decoder->session);
    }
    return status;
}

int anykvm_airplay_video_decode(anykvm_airplay_video *decoder, const uint8_t *data, size_t size,
                                uint8_t **output, int *width, int *height) {
    *output = NULL;
    *width = *height = 0;
    CMBlockBufferRef block = NULL;
    CMSampleBufferRef sample = NULL;
    OSStatus status = CMBlockBufferCreateWithMemoryBlock(
        kCFAllocatorDefault, NULL, size, kCFAllocatorDefault, NULL, 0, size, 0, &block);
    if (status)
        return status;
    status = CMBlockBufferReplaceDataBytes(data, block, 0, size);
    if (!status)
        status = CMSampleBufferCreateReady(kCFAllocatorDefault, block, decoder->format, 1, 0, NULL,
                                           1, &size, &sample);
    if (!status) {
        decoder->status = 0;
        status = VTDecompressionSessionDecodeFrame(decoder->session, sample,
                                                   kVTDecodeFrame_1xRealTimePlayback, NULL, NULL);
        if (!status)
            status = VTDecompressionSessionWaitForAsynchronousFrames(decoder->session);
        if (!status)
            status = decoder->status;
    }
    if (sample)
        CFRelease(sample);
    CFRelease(block);
    if (status)
        return status;
    if (!decoder->image)
        return 0;
    CVPixelBufferRef image = decoder->image;
    decoder->image = NULL;
    *width = (int)CVPixelBufferGetWidth(image);
    *height = (int)CVPixelBufferGetHeight(image);
    if (*width < 1 || *height < 1 || *width > 8192 || *height > 8192) {
        CVPixelBufferRelease(image);
        return -1;
    }
    status = CVPixelBufferLockBaseAddress(image, kCVPixelBufferLock_ReadOnly);
    if (!status) {
        size_t row = (size_t)*width * 4, stride = CVPixelBufferGetBytesPerRow(image);
        const uint8_t *base = CVPixelBufferGetBaseAddress(image);
        if (!base || stride < row ||
            CVPixelBufferGetPixelFormatType(image) != kCVPixelFormatType_32BGRA)
            status = -1;
        else {
            *output = malloc(row * (size_t)*height);
            if (!*output)
                status = -1;
            else
                for (int y = 0; y < *height; y++)
                    memcpy(*output + y * row, base + y * stride, row);
        }
        CVPixelBufferUnlockBaseAddress(image, kCVPixelBufferLock_ReadOnly);
    }
    CVPixelBufferRelease(image);
    return status;
}

void anykvm_airplay_video_close(anykvm_airplay_video *decoder) {
    if (decoder) {
        anykvm_airplay_video_reset(decoder);
        free(decoder);
    }
}
