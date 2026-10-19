#import <CoreMedia/CoreMedia.h>
#import <CoreVideo/CoreVideo.h>
#import <Foundation/Foundation.h>
#include <assert.h>

extern void frame_buf_reset(void);
extern void frame_buf_destroy(void);
extern void frame_buf_store_sample(CMSampleBufferRef);
extern int frame_buf_take(void **, long long *, int);
extern int frame_buf_count(void);

// Exercise the actual native queue with a sample requiring no camera or TCC.
int main(void) {
    @autoreleasepool {
        CVPixelBufferRef pixel = NULL;
        CMVideoFormatDescriptionRef format = NULL;
        CMSampleBufferRef sample = NULL;
        assert(CVPixelBufferCreate(NULL, 64, 48, kCVPixelFormatType_32BGRA, NULL, &pixel) == kCVReturnSuccess);
        assert(CMVideoFormatDescriptionCreateForImageBuffer(NULL, pixel, &format) == noErr);
        CMSampleTimingInfo timing = {CMTimeMake(1, 30), kCMTimeZero, kCMTimeInvalid};
        assert(CMSampleBufferCreateReadyWithImageBuffer(NULL, pixel, format, &timing, &sample) == noErr);
        for (int reopen = 0; reopen < 3; reopen++) {
            frame_buf_destroy();
            int before = frame_buf_count();
            frame_buf_store_sample(sample);
            void *received = NULL;
            long long age = 0;
            assert(frame_buf_count() == before);
            assert(frame_buf_take(&received, &age, 0) == 0);

            frame_buf_reset();
            frame_buf_store_sample(sample);
            assert(frame_buf_count() == before + 1);
            assert(frame_buf_take(&received, &age, 0) == 1);
            assert(received == (void *)sample);
            CFRelease(received);

            // Reset drops unread samples so the next session cannot consume
            // a retained picture from the previous device or codec.
            frame_buf_store_sample(sample);
            frame_buf_reset();
            assert(frame_buf_take(&received, &age, 0) == 0);
        }
        frame_buf_destroy();
        CFRelease(sample);
        CFRelease(format);
        CFRelease(pixel);
    }
    return 0;
}
