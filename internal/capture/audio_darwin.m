#import <AVFoundation/AVFoundation.h>
#import <CoreMedia/CoreMedia.h>
#import <objc/runtime.h>
#import <pthread.h>
#import <stdlib.h>
#import <string.h>
#include "device_types_darwin.h"

// --- Audio ring buffer (shared with Go) ---
// Stores interleaved S16LE PCM produced by the AVFoundation audio tap.
#define AUDIO_RING_CAP (1 << 20) // 1 MiB
static uint8_t g_audio_ring[AUDIO_RING_CAP];
static size_t  g_audio_head = 0; // write pos
static size_t  g_audio_tail = 0; // read pos
static pthread_mutex_t g_audio_mutex = PTHREAD_MUTEX_INITIALIZER;

static size_t audio_ring_used(void) {
    return (g_audio_head - g_audio_tail);
}

static void audio_ring_write(const uint8_t *src, size_t len) {
    pthread_mutex_lock(&g_audio_mutex);
    for (size_t i = 0; i < len; i++) {
        // Drop oldest byte on overflow to keep latency bounded.
        if (audio_ring_used() >= AUDIO_RING_CAP) {
            g_audio_tail++;
        }
        g_audio_ring[g_audio_head % AUDIO_RING_CAP] = src[i];
        g_audio_head++;
    }
    pthread_mutex_unlock(&g_audio_mutex);
}

// audio_buf_read copies up to max bytes into out; returns bytes copied.
int audio_buf_read(uint8_t *out, int max) {
    pthread_mutex_lock(&g_audio_mutex);
    size_t used = audio_ring_used();
    size_t n = (size_t)max < used ? (size_t)max : used;
    for (size_t i = 0; i < n; i++) {
        out[i] = g_audio_ring[(g_audio_tail + i) % AUDIO_RING_CAP];
    }
    g_audio_tail += n;
    pthread_mutex_unlock(&g_audio_mutex);
    return (int)n;
}

void audio_buf_destroy(void) {
    pthread_mutex_lock(&g_audio_mutex);
    g_audio_head = 0;
    g_audio_tail = 0;
    pthread_mutex_unlock(&g_audio_mutex);
}

// --- Audio capture delegate ---
@interface AnyKVMAudioDelegate : NSObject <AVCaptureAudioDataOutputSampleBufferDelegate>
@end

@implementation AnyKVMAudioDelegate

- (void)captureOutput:(AVCaptureOutput *)output
  didOutputSampleBuffer:(CMSampleBufferRef)sampleBuffer
         fromConnection:(AVCaptureConnection *)connection {
    CMBlockBufferRef blockBuf = CMSampleBufferGetDataBuffer(sampleBuffer);
    if (!blockBuf) return;

    size_t len = 0;
    char *dataPtr = NULL;
    if (CMBlockBufferGetDataPointer(blockBuf, 0, NULL, &len, &dataPtr) != kCMBlockBufferNoErr) {
        return;
    }
    if (dataPtr && len > 0) {
        audio_ring_write((const uint8_t *)dataPtr, len);
    }
}

@end

static AVCaptureDevice* audio_find_device(const char *deviceName) {
    AVCaptureDeviceDiscoverySession *disc = [AVCaptureDeviceDiscoverySession
        discoverySessionWithDeviceTypes:@[bk_external_device_type(),
                                           bk_microphone_device_type()]
                              mediaType:AVMediaTypeAudio
                               position:AVCaptureDevicePositionUnspecified];

    AVCaptureDevice *device = nil;
    NSString *name = deviceName ? [NSString stringWithUTF8String:deviceName] : @"";

    if (name.length > 0) {
        for (AVCaptureDevice *d in disc.devices) {
            if ([d.localizedName rangeOfString:name options:NSCaseInsensitiveSearch].location != NSNotFound) {
                device = d;
                break;
            }
        }
    }
    if (!device) {
        int idx = [name intValue];
        if (name.length > 0 && idx >= 0 && idx < (int)disc.devices.count) {
            device = disc.devices[idx];
        }
    }
    if (!device) {
        device = [AVCaptureDevice defaultDeviceWithMediaType:AVMediaTypeAudio];
    }
    return device;
}

// avfoundation_audio_start starts a session capturing S16LE PCM at the given
// sample rate / channel count. Returns a retained session or NULL.
void* avfoundation_audio_start(const char *deviceName, int sampleRate, int channels) {
    @autoreleasepool {
        // Check authorisation (same pattern as the video capture probe).
        AVAuthorizationStatus status = [AVCaptureDevice authorizationStatusForMediaType:AVMediaTypeAudio];
        fprintf(stderr, "audio: mic auth status = %ld\n", (long)status);

        AVCaptureDevice *device = audio_find_device(deviceName);
        if (!device) {
            fprintf(stderr, "audio: no audio device found\n");
            return NULL;
        }
        fprintf(stderr, "audio: selected: %s\n", [device.localizedName UTF8String]);

        AVCaptureSession *session = [[AVCaptureSession alloc] init];
        if (!session) return NULL;

        NSError *err = nil;
        AVCaptureDeviceInput *input = [AVCaptureDeviceInput deviceInputWithDevice:device error:&err];
        if (input) {
            if ([session canAddInput:input]) {
                [session addInput:input];
            } else {
                fprintf(stderr, "audio: cannot add input (session rejected)\n");
                return NULL;
            }
        } else {
            const char *reason = err ? [err.localizedDescription UTF8String] : "unknown";
            fprintf(stderr, "audio: cannot create input (status=%ld): %s\n", (long)status, reason);
            return NULL;
        }

        AVCaptureAudioDataOutput *output = [[AVCaptureAudioDataOutput alloc] init];
        output.audioSettings = @{
            AVFormatIDKey:            @(kAudioFormatLinearPCM),
            AVSampleRateKey:          @(sampleRate),
            AVNumberOfChannelsKey:    @(channels),
            AVLinearPCMBitDepthKey:   @(16),
            AVLinearPCMIsFloatKey:    @NO,
            AVLinearPCMIsBigEndianKey:@NO,
            AVLinearPCMIsNonInterleaved:@NO,
        };

        AnyKVMAudioDelegate *delegate = [[AnyKVMAudioDelegate alloc] init];
        dispatch_queue_t queue = dispatch_queue_create("anykvm.audio", DISPATCH_QUEUE_SERIAL);
        [output setSampleBufferDelegate:delegate queue:queue];

        if (![session canAddOutput:output]) {
            fprintf(stderr, "audio: cannot add output\n");
            return NULL;
        }
        [session addOutput:output];

        // Keep the delegate alive for the session lifetime.
        objc_setAssociatedObject(session, "anykvm_audio_delegate", delegate,
                                 OBJC_ASSOCIATION_RETAIN);

        [session startRunning];
        fprintf(stderr, "audio: session started (%dHz %dch)\n", sampleRate, channels);
        return (void *)CFBridgingRetain(session);
    }
}

void avfoundation_audio_stop(void *session) {
    if (session) {
        AVCaptureSession *s = (AVCaptureSession *)CFBridgingRelease(session);
        [s stopRunning];
    }
}

// avfoundation_list_audio_devices writes available audio input device names
// into buf (maxLen bytes) in the same "idx\0name\n" format.
void avfoundation_list_audio_devices(char *buf, int maxLen) {
    @autoreleasepool {
        AVCaptureDeviceDiscoverySession *disc = [AVCaptureDeviceDiscoverySession
            discoverySessionWithDeviceTypes:@[bk_microphone_device_type(),
                                               bk_external_device_type()]
                                  mediaType:AVMediaTypeAudio
                                   position:AVCaptureDevicePositionUnspecified];

        int pos = 0;
        for (int i = 0; i < (int)disc.devices.count && pos + 256 < maxLen; i++) {
            AVCaptureDevice *d = disc.devices[i];
            const char *name = [d.localizedName UTF8String];
            pos += snprintf(buf + pos, maxLen - pos, "%d\t%s\n", i, name);
        }
        if (pos == 0) buf[0] = '\0';
    }
}
