// Synthetic native samples exercise actual capture ownership, stride handling,
// JPEG encoding and asynchronous VideoToolbox submission. No camera is opened.
#import "../../../../internal/capture/capture_darwin.m"
#import "../../../../internal/videoenc/native.h"
#import <ImageIO/ImageIO.h>
#include <assert.h>

CFDataRef bk_jpeg_encode_pixel(CVPixelBufferRef pixel, int quality);
int bk_jpeg_decode(const uint8_t *data, size_t size, uint8_t *pixels, int width, int height);

static CMSampleBufferRef sample(OSType type, size_t w, size_t h) {
    CVPixelBufferRef pixel = NULL;
    NSDictionary *attrs = @{
        (id)kCVPixelBufferBytesPerRowAlignmentKey : @64,
        (id)kCVPixelBufferIOSurfacePropertiesKey : @{}
    };
    assert(!CVPixelBufferCreate(NULL, w, h, type, (CFDictionaryRef)attrs, &pixel));
    assert(!CVPixelBufferLockBaseAddress(pixel, 0));
    if (type == kCVPixelFormatType_32BGRA) {
        size_t stride = CVPixelBufferGetBytesPerRow(pixel);
        for (size_t y = 0; y < h; y++) {
            uint8_t *row = CVPixelBufferGetBaseAddress(pixel) + y * stride;
            memset(row, 0xee, stride);
            for (size_t x = 0; x < w; x++) {
                row[4 * x] = y < h / 2 ? 0 : 240;
                row[4 * x + 1] = 20;
                row[4 * x + 2] = y < h / 2 ? 240 : 0;
                row[4 * x + 3] = 255;
            }
        }
    } else {
        for (int plane = 0; plane < 2; plane++) {
            size_t stride = CVPixelBufferGetBytesPerRowOfPlane(pixel, plane),
                   height = CVPixelBufferGetHeightOfPlane(pixel, plane);
            memset(CVPixelBufferGetBaseAddressOfPlane(pixel, plane), plane ? 128 : 80,
                   stride * height);
        }
    }
    CVPixelBufferUnlockBaseAddress(pixel, 0);
    CMVideoFormatDescriptionRef format = NULL;
    assert(!CMVideoFormatDescriptionCreateForImageBuffer(NULL, pixel, &format));
    CMSampleTimingInfo timing = {CMTimeMake(1, 60), CMClockGetTime(CMClockGetHostTimeClock()),
                                 kCMTimeInvalid};
    CMSampleBufferRef result = NULL;
    assert(!CMSampleBufferCreateReadyWithImageBuffer(NULL, pixel, format, &timing, &result));
    CFRelease(format);
    CVPixelBufferRelease(pixel);
    return result;
}

static void pixels_and_jpeg(void) {
    frame_buf_reset();
    CMSampleBufferRef older = sample(kCVPixelFormatType_32BGRA, 322, 242);
    frame_buf_store_sample(older);
    CFRelease(older);
    CMSampleBufferRef newest = sample(kCVPixelFormatType_32BGRA, 322, 242);
    frame_buf_store_sample(newest);
    CFRelease(newest);
    void *lease = NULL;
    long long age = 0;
    assert(frame_buf_take(&lease, &age, 0));
    assert(lease == newest && age >= 0);
    frame_buf_destroy(); // the taken lease remains valid after queue shutdown
    int w, h, tag;
    size_t size = capture_sample_layout(lease, 0, &w, &h, &tag);
    assert(w == 322 && h == 242 && tag == 1 && size == 322 * 242 * 4);
    uint8_t *pixels = malloc(size + 32);
    memset(pixels, 0xa5, size + 32);
    assert(capture_sample_copy(lease, tag, pixels + 16, size));
    for (int i = 0; i < 16; i++)
        assert(pixels[i] == 0xa5 && pixels[size + 16 + i] == 0xa5);
    assert(pixels[16] == 0 && pixels[18] == 240 && pixels[16 + (h - 1) * w * 4] == 240);
    CFDataRef jpeg = bk_jpeg_encode_pixel(capture_sample_pixel(lease), 90);
    assert(jpeg && CFDataGetLength(jpeg) > 100);
    CGImageSourceRef source = CGImageSourceCreateWithData(jpeg, NULL);
    CGImageRef image = CGImageSourceCreateImageAtIndex(source, 0, NULL);
    assert(image && CGImageGetWidth(image) == 322 && CGImageGetHeight(image) == 242);
    uint8_t *decoded = malloc(size);
    assert(bk_jpeg_decode(CFDataGetBytePtr(jpeg), CFDataGetLength(jpeg), decoded, w, h));
    assert(decoded[2] > 200 && decoded[0] < 40);
    assert(decoded[(h - 1) * w * 4] > 200 && decoded[(h - 1) * w * 4 + 2] < 40);
    free(decoded);
    assert(capture_sample_copy(lease, tag, pixels + 16, size)); // JPEG did not mutate input
    assert(pixels[18] == 240 && pixels[16 + (h - 1) * w * 4] == 240);
    CGImageRelease(image);
    CFRelease(source);
    CFRelease(jpeg);
    free(pixels);
    capture_sample_release(lease);
    frame_buf_reset();
    CMSampleBufferRef nv12 = sample(kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange, 320, 240);
    frame_buf_store_sample(nv12);
    CFRelease(nv12);
    assert(frame_buf_take(&lease, &age, 0));
    size = capture_sample_layout(lease, 1, &w, &h, &tag);
    assert(tag == 4 && size == 320 * 240 * 3 / 2);
    pixels = malloc(size);
    assert(capture_sample_copy(lease, tag, pixels, size));
    assert(pixels[0] == 80 && pixels[320 * 240 - 1] == 80 && pixels[320 * 240] == 128 &&
           pixels[size - 1] == 128);
    jpeg = bk_jpeg_encode_pixel(capture_sample_pixel(lease), 80);
    assert(jpeg);
    CFRelease(jpeg);
    free(pixels);
    capture_sample_release(lease);
    frame_buf_destroy();
    lease = NULL;
    assert(!frame_buf_take(&lease, &age, 0));
    puts(
        "native buffers: newest sample, owned lease, padded BGRA/NV12, direct JPEG, shutdown passed");
}

// A compressed sample remains readable after capture's queue releases its copy.
static void compressed_borrow(void) {
    CMSampleBufferRef raw = sample(kCVPixelFormatType_32BGRA, 320, 240);
    CFDataRef jpeg = bk_jpeg_encode_pixel(capture_sample_pixel(raw), 80);
    CFRelease(raw);
    size_t size = CFDataGetLength(jpeg);
    CMBlockBufferRef block = NULL;
    assert(!CMBlockBufferCreateWithMemoryBlock(NULL, NULL, size, kCFAllocatorDefault, NULL, 0, size,
                                               0, &block));
    assert(!CMBlockBufferReplaceDataBytes(CFDataGetBytePtr(jpeg), block, 0, size));
    CMVideoFormatDescriptionRef format = NULL;
    assert(!CMVideoFormatDescriptionCreate(NULL, kCMVideoCodecType_JPEG, 320, 240, NULL, &format));
    CMSampleTimingInfo timing = {CMTimeMake(1, 60), CMClockGetTime(CMClockGetHostTimeClock()),
                                 kCMTimeInvalid};
    CMSampleBufferRef compressed = NULL;
    assert(!CMSampleBufferCreateReady(NULL, block, format, 1, 1, &timing, 1, &size, &compressed));
    CFRelease(block);
    CFRelease(format);
    frame_buf_reset();
    frame_buf_store_sample(compressed);
    CFRelease(compressed);
    void *lease = NULL;
    long long age = 0;
    assert(frame_buf_take(&lease, &age, 0));
    frame_buf_destroy();
    const uint8_t *data = capture_sample_data(lease, size);
    assert(data && !memcmp(data, CFDataGetBytePtr(jpeg), size));
    capture_sample_release(lease);
    CFRelease(jpeg);
    puts("compressed sample: contiguous borrowed bytes survive capture shutdown");
}

static void asynchronous(int hevc, const char *prefix) {
    char error[512] = {0};
    bk_encoder *encoder = bk_encoder_open("videotoolbox", "", 320, 240, 60, 1, 2000, hevc, 0, 0,
                                          error, sizeof(error));
    if (!encoder) {
        fprintf(stderr, "encoder unavailable: %s\n", error);
        abort();
    }
    FILE *stream = NULL;
    if (prefix) {
        char name[4096];
        snprintf(name, sizeof(name), "%s.%s", prefix, hevc ? "hevc" : "h264");
        stream = fopen(name, "wb");
        assert(stream);
    }
    int limit = hevc ? 1 : 2;
    // Alternate native formats; release each source immediately after submission.
    // Native NV12 is passed directly, BGRA exercises the native pixel transfer.
    for (int i = 1; i <= limit; i++) {
        CMSampleBufferRef input = sample(i % 2 ? kCVPixelFormatType_32BGRA
                                               : kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange,
                                         320, 240);
        int result = bk_encoder_submit_pixel(encoder, capture_sample_pixel(input), i, i == 1, error,
                                             sizeof(error));
        if (result != 1) {
            fprintf(stderr, "submit: %s\n", error);
            abort();
        }
        CFRelease(input);
    }
    CMSampleBufferRef input = sample(kCVPixelFormatType_32BGRA, 320, 240);
    assert(bk_encoder_submit_pixel(encoder, capture_sample_pixel(input), 999, 0, error,
                                   sizeof(error)) == 0);
    CFRelease(input);
    void *held = NULL;
    const uint8_t *heldData = NULL;
    size_t heldSize = 0;
    uint8_t *snapshot = NULL;
    int next = limit + 1, received = 0;
    int64_t deadline = capture_host_ns() + 5 * NSEC_PER_SEC;
    while (received < 32 && capture_host_ns() < deadline) {
        void *lease = NULL;
        const uint8_t *data = NULL;
        size_t size = 0;
        uint64_t frameID = 0;
        int dropped = 0;
        int result = bk_encoder_receive(encoder, &lease, &data, &size, &frameID, &dropped, error,
                                        sizeof(error));
        if (result < 0) {
            fprintf(stderr, "receive: %s\n", error);
            abort();
        }
        if (!result) {
            usleep(1000);
            continue;
        }
        received++;
        assert(frameID == (uint64_t)received && !dropped && size > 5 &&
               !memcmp(data, "\0\0\0\1", 4));
        if (stream)
            assert(fwrite(data, 1, size, stream) == size);
        if (received == 1) {
            held = lease;
            heldData = data;
            heldSize = size;
            snapshot = malloc(size);
            memcpy(snapshot, data, size);
        } else {
            bk_encoder_packet_release(lease);
        }
        input = sample(next % 2 ? kCVPixelFormatType_32BGRA
                                : kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange,
                       320, 240);
        assert(bk_encoder_submit_pixel(encoder, capture_sample_pixel(input), next, next == 12,
                                       error, sizeof(error)) == 1);
        CFRelease(input);
        next++;
    }
    assert(received == 32);
    bk_encoder_close(encoder); // close with in-flight buffers still retained
    assert(held && !memcmp(heldData, snapshot, heldSize));
    bk_encoder_packet_release(held);
    free(snapshot);
    if (stream)
        fclose(stream);
    printf(
        "native %s: bounded asynchronous submit, ordered callbacks, early source release and close passed\n",
        hevc ? "HEVC" : "H264");
}
int main(int argc, char **argv) {
    @autoreleasepool {
        pixels_and_jpeg();
        compressed_borrow();
        asynchronous(0, argc > 1 ? argv[1] : NULL);
        asynchronous(1, argc > 1 ? argv[1] : NULL);
        return 0;
    }
}
