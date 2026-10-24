#import <AVFoundation/AVFoundation.h>
#import <CoreMedia/CoreMedia.h>
#import <CoreMediaIO/CMIOHardware.h>
#import <CoreGraphics/CoreGraphics.h>
#import <CoreVideo/CoreVideo.h>
#import <pthread.h>
#import <stdlib.h>
#import <string.h>
#import <unistd.h>
#include <math.h>
#include "device_types_darwin.h"

// iOS USB screen-mirror discovery timing (see cmio_warm_ios_dal).
#define IOS_WARMUP_MAX_MS     2000  // max DAL warm poll while AVCaptureScreenInput runs
#define IOS_WARMUP_POLL_MS      80  // poll / run-loop slice during warm
#define IOS_RESOLVE_WAIT_MS   8000  // muxed publish wait for ios-usb discovery
#define IOS_DISCOVER_POLL_MS   250  // run-loop slice while waiting for muxed publish

// --- Latest native sample, transferred to Go as an owned lease ---
#include "pixels_darwin.h"
#include <sys/time.h>

static CMSampleBufferRef g_frame_sample;
static int64_t g_frame_arrival;
static int g_frame_closed;
static int g_frame_count;
static pthread_mutex_t g_frame_mutex = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t g_frame_condition = PTHREAD_COND_INITIALIZER;

static int64_t capture_host_ns(void) {
    return CMTimeConvertScale(CMClockGetTime(CMClockGetHostTimeClock()), NSEC_PER_SEC, kCMTimeRoundingMethod_Default).value;
}

void frame_buf_store_sample(CMSampleBufferRef sample) {
    if (!sample || !CMSampleBufferIsValid(sample)) return;
    CFRetain(sample);
    pthread_mutex_lock(&g_frame_mutex);
    CMSampleBufferRef old = g_frame_sample;
    if (!g_frame_closed) {
        g_frame_sample = sample;
        // Device PTS may use a different clock (notably USB iOS mirrors).
        g_frame_arrival = capture_host_ns();
        g_frame_count++;
        pthread_cond_signal(&g_frame_condition);
    } else { CFRelease(sample); old = NULL; }
    pthread_mutex_unlock(&g_frame_mutex);
    if (old) CFRelease(old);
}

int frame_buf_take_timed(void **sample, long long *age_ns, long long *pts_us, int wait_ms) {
    *pts_us = 0;
    *sample = NULL; *age_ns = 0;
    struct timeval now; gettimeofday(&now, NULL);
    struct timespec deadline = {now.tv_sec, now.tv_usec * 1000 + wait_ms * NSEC_PER_MSEC};
    if (deadline.tv_nsec >= NSEC_PER_SEC) { deadline.tv_sec++; deadline.tv_nsec -= NSEC_PER_SEC; }
    pthread_mutex_lock(&g_frame_mutex);
    while (wait_ms > 0 && !g_frame_sample && !g_frame_closed) {
        if (pthread_cond_timedwait(&g_frame_condition, &g_frame_mutex, &deadline)) break;
    }
    if (g_frame_sample) {
        *sample = (void *)g_frame_sample;
        *age_ns = capture_host_ns() - g_frame_arrival;
        // Device PTS may have an unknown clock. Retain callback arrival on the
        // native host clock instead of reconstructing playback time in Go.
        *pts_us = g_frame_arrival / 1000;
        g_frame_sample = NULL; // Transfer the queue's retain to the caller.
    }
    pthread_mutex_unlock(&g_frame_mutex);
    return *sample != NULL;
}

// Keep the existing native queue API for capture tools that only need age.
int frame_buf_take(void **sample, long long *age_ns, int wait_ms) {
    long long pts_us;
    return frame_buf_take_timed(sample, age_ns, &pts_us, wait_ms);
}

void frame_buf_reset(void) {
    pthread_mutex_lock(&g_frame_mutex);
    if (g_frame_sample) CFRelease(g_frame_sample);
    g_frame_sample = NULL;
    g_frame_closed = 0;
    pthread_mutex_unlock(&g_frame_mutex);
}
void frame_buf_destroy(void) {
    pthread_mutex_lock(&g_frame_mutex);
    g_frame_closed = 1;
    if (g_frame_sample) CFRelease(g_frame_sample);
    g_frame_sample = NULL;
    pthread_cond_broadcast(&g_frame_condition);
    pthread_mutex_unlock(&g_frame_mutex);
}
int frame_buf_count(void) {
    pthread_mutex_lock(&g_frame_mutex);
    int count = g_frame_count;
    pthread_mutex_unlock(&g_frame_mutex);
    return count;
}
void capture_sample_release(void *sample) { if (sample) CFRelease(sample); }
void *capture_sample_pixel(void *sample) { return (void *)CMSampleBufferGetImageBuffer(sample); }
size_t capture_sample_layout(void *sample, int passthrough, int *w, int *h, int *tag) {
    CVPixelBufferRef pixel = CMSampleBufferGetImageBuffer(sample);
    if (pixel) {
        *w = (int)CVPixelBufferGetWidth(pixel); *h = (int)CVPixelBufferGetHeight(pixel);
        return bk_pixel_layout(pixel, passthrough, tag);
    }
    CMFormatDescriptionRef format = CMSampleBufferGetFormatDescription(sample);
    CMVideoDimensions dims = CMVideoFormatDescriptionGetDimensions(format);
    *w = dims.width; *h = dims.height; *tag = 0;
    CMBlockBufferRef block = CMSampleBufferGetDataBuffer(sample);
    return block ? CMBlockBufferGetDataLength(block) : 0;
}
// Borrow contiguous compressed bytes; the retained CMSampleBuffer owns storage.
void *capture_sample_data(void *sample, size_t size) {
    CMBlockBufferRef block = CMSampleBufferGetDataBuffer(sample);
    size_t length = 0, total = 0; char *data = NULL;
    if (!block || CMBlockBufferGetDataPointer(block, 0, &length, &total, &data) != noErr || length != size || total != size) return NULL;
    return data;
}
int capture_sample_copy(void *sample, int tag, uint8_t *dest, size_t size) {
    @autoreleasepool {
        CVPixelBufferRef pixel = CMSampleBufferGetImageBuffer(sample);
        if (pixel) return bk_pixel_copy(pixel, tag, dest, size);
        CMBlockBufferRef block = CMSampleBufferGetDataBuffer(sample);
        return block && size == CMBlockBufferGetDataLength(block) && CMBlockBufferCopyDataBytes(block, 0, size, dest) == noErr;
    }
}

@interface GoKVMCaptureDelegate : NSObject <AVCaptureVideoDataOutputSampleBufferDelegate>
{
    BOOL loggedFirstSample;
}
@end
@implementation GoKVMCaptureDelegate
- (void)captureOutput:(AVCaptureOutput *)output didOutputSampleBuffer:(CMSampleBufferRef)sample fromConnection:(AVCaptureConnection *)connection {
    (void)output; (void)connection;
    if (!sample) return;
    if (!loggedFirstSample) {
        loggedFirstSample = YES;
        fprintf(stderr, "capture: first AVFoundation sample (valid=%d pixel_buffer=%d)\n",
                CMSampleBufferIsValid(sample), CMSampleBufferGetImageBuffer(sample) != NULL);
    }
    // No CPU lock, pixel conversion or allocation in the capture callback.
    frame_buf_store_sample(sample);
}
@end

// Own callbacks and observe the actual capture session, without a global
// observer that could outlive it or mistake DAL warm-up for phone capture.
// A separate retained audio lease survives video shutdown until its reader closes.
@interface BKUSBAudio : NSObject <AVCaptureAudioDataOutputSampleBufferDelegate> {
@public
    pthread_mutex_t mutex;
    uint8_t pcm[19200]; // bounded to 100ms at 48kHz / stereo / S16LE
    size_t used;
    BOOL subscribed, closed, loggedFormat;
}
@end
@implementation BKUSBAudio
- (instancetype)init {
    self=[super init]; if(self) pthread_mutex_init(&mutex,NULL); return self;
}
- (void)captureOutput:(AVCaptureOutput *)output didOutputSampleBuffer:(CMSampleBufferRef)sample fromConnection:(AVCaptureConnection *)connection {
    (void)output; (void)connection;
    pthread_mutex_lock(&mutex);
    if(!subscribed || closed) {pthread_mutex_unlock(&mutex);return;}
    const AudioStreamBasicDescription *f=CMAudioFormatDescriptionGetStreamBasicDescription(CMSampleBufferGetFormatDescription(sample));
    BOOL valid=f && f->mFormatID==kAudioFormatLinearPCM && f->mSampleRate==48000 && f->mChannelsPerFrame==2 &&
        f->mBitsPerChannel==16 && f->mBytesPerFrame==4 &&
        (f->mFormatFlags & kAudioFormatFlagIsSignedInteger) &&
        !(f->mFormatFlags & (kAudioFormatFlagIsFloat|kAudioFormatFlagIsBigEndian|kAudioFormatFlagIsNonInterleaved));
    if(!loggedFormat) {
        loggedFormat=YES;
        fprintf(stderr,"capture: USB audio sample rate=%.0f channels=%u bits=%u flags=%u valid_pcm=%d\n",
            f?f->mSampleRate:0,f?(unsigned)f->mChannelsPerFrame:0,f?(unsigned)f->mBitsPerChannel:0,f?(unsigned)f->mFormatFlags:0,valid);
    }
    CMBlockBufferRef block=CMSampleBufferGetDataBuffer(sample);
    if(valid && block && CMSampleBufferIsValid(sample)) {
        size_t size=CMBlockBufferGetDataLength(block)&~(size_t)3, offset=0;
        if(size>sizeof(pcm)) {offset=size-sizeof(pcm);size=sizeof(pcm);}
        if(used+size>sizeof(pcm)) {size_t drop=used+size-sizeof(pcm);memmove(pcm,pcm+drop,used-drop);used-=drop;}
        if(CMBlockBufferCopyDataBytes(block,offset,size,pcm+used)==noErr) used+=size;
    }
    pthread_mutex_unlock(&mutex);
}
- (void)dealloc {pthread_mutex_destroy(&mutex);[super dealloc];}
@end

@interface GoKVMCaptureSession : AVCaptureSession
@property(nonatomic, retain) GoKVMCaptureDelegate *frameDelegate;
@property(nonatomic, retain) BKUSBAudio *audioDelegate;
@end
@implementation GoKVMCaptureSession
@synthesize frameDelegate;
@synthesize audioDelegate;
- (instancetype)init {
    self = [super init];
    if (self) {
        NSNotificationCenter *center = [NSNotificationCenter defaultCenter];
        [center addObserver:self selector:@selector(captureState:) name:AVCaptureSessionRuntimeErrorNotification object:self];
        [center addObserver:self selector:@selector(captureState:) name:AVCaptureSessionWasInterruptedNotification object:self];
        [center addObserver:self selector:@selector(captureState:) name:AVCaptureSessionInterruptionEndedNotification object:self];
    }
    return self;
}
- (void)captureState:(NSNotification *)notification {
    NSError *error = notification.userInfo[AVCaptureSessionErrorKey];
    fprintf(stderr, "capture: AVFoundation %s running=%d error=%s code=%ld domain=%s\n",
            [notification.name UTF8String], self.isRunning,
            [error.localizedDescription UTF8String] ?: "none", (long)error.code,
            [error.domain UTF8String] ?: "none");
}
- (void)dealloc {
    [[NSNotificationCenter defaultCenter] removeObserver:self];
    [frameDelegate release];
    [audioDelegate release];
    [super dealloc];
}
@end

// --- Device discovery ---

// cmio_allow_screen_capture_devices opts this process in to CoreMediaIO DAL
// screen-capture devices (fast — property set only).
static void cmio_allow_screen_capture_devices(void) {
    static dispatch_once_t once;
    dispatch_once(&once, ^{
        CMIOObjectPropertyAddress prop = {
            kCMIOHardwarePropertyAllowScreenCaptureDevices,
            kCMIOObjectPropertyScopeGlobal,
            kCMIOObjectPropertyElementMain
        };
        UInt32 allow = 1;
        OSStatus st = CMIOObjectSetPropertyData(kCMIOObjectSystemObject, &prop,
                                                0, NULL, sizeof(allow), &allow);
        if (st != 0) {
            fprintf(stderr, "capture: enable screen-capture devices failed (status=%d)\n", (int)st);
        }
    });
}

static NSArray<AVCaptureDevice *> *enumerate_devices(void);
static int count_muxed_devices(NSArray<AVCaptureDevice *> *arr);

// muxed_devices_visible returns 1 if any iOS screen-mirror (muxed) device is listed.
static int muxed_devices_visible(void) {
    return count_muxed_devices(enumerate_devices()) > 0;
}

// --- DAL warm session (AVCaptureScreenInput keeps iOSScreenCaptureAssistant alive) ---

static AVCaptureSession *g_dal_warm_session = nil;
static AVCaptureScreenInput *g_dal_warm_input = nil;

static void cmio_warm_session_stop(void) {
    if (g_dal_warm_session) {
        [g_dal_warm_session stopRunning];
        [g_dal_warm_session release];
        g_dal_warm_session = nil;
    }
    if (g_dal_warm_input) {
        [g_dal_warm_input release];
        g_dal_warm_input = nil;
    }
}

// cmio_warm_session_start ensures the DAL warm session is running. Returns 1 if
// AVCaptureScreenInput was added and the session is running.
static int cmio_warm_session_start(void) {
    cmio_allow_screen_capture_devices();

    static dispatch_once_t tccOnce;
    dispatch_once(&tccOnce, ^{
        bool screenOK = false;
        if (@available(macOS 10.15, *)) {
            screenOK = CGPreflightScreenCaptureAccess();
            if (!screenOK) {
                fprintf(stderr, "capture: Screen Recording not granted; requesting access for DAL warm-up\n");
                screenOK = CGRequestScreenCaptureAccess();
            }
        }
        fprintf(stderr, "capture: Screen Recording access %s\n",
                screenOK ? "granted" : "denied/unknown");
    });

    if (g_dal_warm_session && g_dal_warm_session.isRunning) {
        return 1;
    }
    cmio_warm_session_stop();

    @autoreleasepool {
        AVCaptureSession *session = [[AVCaptureSession alloc] init];
        AVCaptureScreenInput *input = [[AVCaptureScreenInput alloc]
            initWithDisplayID:CGMainDisplayID()];
        input.capturesCursor = NO;
        input.capturesMouseClicks = NO;
        input.minFrameDuration = CMTimeMake(1, 1); // 1fps - just enough to load DAL
        BOOL added = NO;
        if ([session canAddInput:input]) {
            [session addInput:input];
            added = YES;
            fprintf(stderr, "capture: DAL warm-up via AVCaptureScreenInput started\n");
        } else {
            fprintf(stderr,
                    "capture: AVCaptureScreenInput canAddInput=NO; "
                    "grant Screen Recording or open QuickTime once\n");
        }
        [session startRunning];
        g_dal_warm_session = [session retain];
        g_dal_warm_input = [input retain];
        [session release];
        [input release];
        return added;
    }
}

// cmio_warm_session_poll spins the run loop with the warm session running for up to
// max_ms. Returns 1 if a muxed device is visible.
static int cmio_warm_session_poll(int max_ms) {
    if (max_ms <= 0) {
        return muxed_devices_visible();
    }
    if (!cmio_warm_session_start()) {
        return muxed_devices_visible();
    }
    NSDate *deadline = [NSDate dateWithTimeIntervalSinceNow:max_ms / 1000.0];
    while ([deadline timeIntervalSinceNow] > 0) {
        if (muxed_devices_visible()) {
            return 1;
        }
        [[NSRunLoop currentRunLoop] runMode:NSDefaultRunLoopMode
                                 beforeDate:[NSDate dateWithTimeIntervalSinceNow:
                                                     IOS_WARMUP_POLL_MS / 1000.0]];
    }
    return muxed_devices_visible();
}

// cmio_warm_ios_dal starts the warm session and polls briefly. The session stays
// running so a follow-up wait_muxed_publish can keep the DAL loaded.
static void cmio_warm_ios_dal(void) {
    if (g_dal_warm_session && g_dal_warm_session.isRunning) {
        return;
    }
    int muxed = cmio_warm_session_poll(IOS_WARMUP_MAX_MS);
    fprintf(stderr, "capture: DAL warm-up done (muxed visible=%d)\n", muxed);
}

// enable_screen_capture_devices opts in and fully warms DAL (ios capture path).
static void enable_screen_capture_devices(void) {
    cmio_warm_ios_dal();
}

// enumerate_devices merges Video + Muxed discovery sessions (no CMIO side effects).
static NSArray<AVCaptureDevice *> *enumerate_devices(void) {
    AVCaptureDeviceDiscoverySession *vid = [AVCaptureDeviceDiscoverySession
        discoverySessionWithDeviceTypes:@[bk_external_device_type(), AVCaptureDeviceTypeBuiltInWideAngleCamera]
                              mediaType:AVMediaTypeVideo
                               position:AVCaptureDevicePositionUnspecified];
    AVCaptureDeviceDiscoverySession *mux = [AVCaptureDeviceDiscoverySession
        discoverySessionWithDeviceTypes:@[bk_external_device_type()]
                              mediaType:AVMediaTypeMuxed
                               position:AVCaptureDevicePositionUnspecified];
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
    NSArray<AVCaptureDevice *> *legacyMux = [AVCaptureDevice devicesWithMediaType:AVMediaTypeMuxed];
#pragma clang diagnostic pop

    NSMutableArray<AVCaptureDevice *> *out = [NSMutableArray array];
    NSMutableSet<NSString *> *seen = [NSMutableSet set];
    for (NSArray<AVCaptureDevice *> *arr in @[vid.devices, mux.devices, legacyMux]) {
        for (AVCaptureDevice *d in arr) {
            if ([seen containsObject:d.uniqueID]) continue;
            [seen addObject:d.uniqueID];
            [out addObject:d];
        }
    }
    return out;
}

static int count_muxed_devices(NSArray<AVCaptureDevice *> *arr) {
    int n = 0;
    for (AVCaptureDevice *d in arr) {
        if ([d hasMediaType:AVMediaTypeMuxed]) n++;
    }
    return n;
}

static NSArray<AVCaptureDevice *> *discover_video_devices(void);

// wait_muxed_publish blocks up to wait_ms for an iOS USB mirror (muxed device) to
// appear. Same discovery loop as -video avf resolve — no idle early exit.
// Keeps the DAL warm session running for the whole wait.
// Returns 1 if a muxed device is visible before the deadline.
static int wait_muxed_publish(int wait_ms) {
    if (muxed_devices_visible()) {
        return 1;
    }
    if (wait_ms <= 0) {
        return 0;
    }

    if (!cmio_warm_session_start()) {
        return muxed_devices_visible();
    }
    fprintf(stderr, "capture: waiting for iOS mirror (up to %dms)\n", wait_ms);

    __block int found = 0;
    NSString *mode = @"anykvm.ios.discover";
    id obs = [[NSNotificationCenter defaultCenter]
        addObserverForName:AVCaptureDeviceWasConnectedNotification
                    object:nil
                     queue:nil
                usingBlock:^(NSNotification *note) {
                    AVCaptureDevice *d = note.object;
                    if (![d isKindOfClass:[AVCaptureDevice class]]) return;
                    if (![d hasMediaType:AVMediaTypeMuxed]) return;
                    found = 1;
                    CFRunLoopStop(CFRunLoopGetCurrent());
                }];

    NSDate *deadline = [NSDate dateWithTimeIntervalSinceNow:wait_ms / 1000.0];
    (void)enumerate_devices();
    while (!found && [deadline timeIntervalSinceNow] > 0) {
        if (muxed_devices_visible()) {
            found = 1;
            break;
        }
        (void)enumerate_devices();
        NSTimeInterval slice = MIN(IOS_DISCOVER_POLL_MS / 1000.0, [deadline timeIntervalSinceNow]);
        if (slice <= 0) break;
        [[NSRunLoop currentRunLoop] runMode:mode beforeDate:[NSDate dateWithTimeIntervalSinceNow:slice]];
        [[NSRunLoop currentRunLoop] runMode:NSDefaultRunLoopMode
                                 beforeDate:[NSDate dateWithTimeIntervalSinceNow:0.05]];
    }

    [[NSNotificationCenter defaultCenter] removeObserver:obs];
    int ok = found || muxed_devices_visible();
    cmio_warm_session_stop();
    fprintf(stderr, "capture: iOS mirror wait done (found=%d)\n", ok);
    return ok;
}

// discover_video_devices_for_list warms DAL, then uses the same muxed publish wait
// as ios-usb capture. Exits as soon as iPhone/iPad appears.
static NSArray<AVCaptureDevice *> *discover_video_devices_for_list(void) {
    cmio_warm_ios_dal();
    NSArray<AVCaptureDevice *> *devices = enumerate_devices();
    if (count_muxed_devices(devices) > 0) {
        cmio_warm_session_stop();
        return devices;
    }
    if (wait_muxed_publish(IOS_RESOLVE_WAIT_MS)) {
        devices = enumerate_devices();
    } else {
        cmio_warm_session_stop();
    }
    return devices;
}

// discover_video_devices fully warms DAL then enumerates (ios capture path).
static NSArray<AVCaptureDevice *> *discover_video_devices(void) {
    enable_screen_capture_devices();
    return enumerate_devices();
}

// copy_matching_ios_name writes the first muxed device matching sel into out.
// Returns 1 on match, 0 otherwise.
static int copy_matching_ios_name(NSString *sel, char *out, int outlen) {
    for (AVCaptureDevice *d in discover_video_devices()) {
        if (![d hasMediaType:AVMediaTypeMuxed]) continue;
        if (sel.length > 0 &&
            [d.localizedName rangeOfString:sel options:NSCaseInsensitiveSearch].location == NSNotFound) {
            continue;
        }
        const char *name = [d.localizedName UTF8String];
        if (!name) return 0;
        strncpy(out, name, outlen - 1);
        out[outlen - 1] = '\0';
        return 1;
    }
    return 0;
}

// avfoundation_resolve_ios_wait finds an iOS screen-mirroring device (iPhone/iPad
// over USB). selector optionally filters by localizedName substring (case
// insensitive); empty selects the first iOS device. On success copies the
// device's localizedName into out and returns 1; returns 0 when no device matched.
//
// wait_ms > 0: after the first miss, block up to wait_ms listening for
// AVCaptureDeviceWasConnected while re-polling. wait_ms == 0: single shot.
// CMIO warm-up runs via discover_video_devices() on the first enumeration.
int avfoundation_resolve_ios_wait(const char *selector, char *out, int outlen, int wait_ms) {
    @autoreleasepool {
        NSString *sel = selector ? [NSString stringWithUTF8String:selector] : @"";
        if (copy_matching_ios_name(sel, out, outlen)) {
            return 1;
        }
        if (wait_ms <= 0) {
            return 0;
        }
        if (wait_muxed_publish(wait_ms)) {
            return copy_matching_ios_name(sel, out, outlen);
        }
        return 0;
    }
}

// --- Public API ---
void avfoundation_stop(void *session);
void* avfoundation_start(const char *deviceName, int width, int height, double fps, unsigned int pixel_format) {

    AVAuthorizationStatus authStatus = [AVCaptureDevice authorizationStatusForMediaType:AVMediaTypeVideo];
    fprintf(stderr, "capture: camera auth status = %ld\n", (long)authStatus);
    if (authStatus == AVAuthorizationStatusDenied || authStatus == AVAuthorizationStatusRestricted) {
        fprintf(stderr, "capture: camera access denied\n");
        return NULL;
    }

    // Discover all video-capable devices (cameras, capture cards, iOS mirrors)
    NSArray<AVCaptureDevice *> *devices = discover_video_devices();

    static BOOL listPrinted = NO;
    if (!listPrinted) {
        listPrinted = YES;
        fprintf(stderr, "capture: %d device(s):\n", (int)devices.count);
        for (int i = 0; i < (int)devices.count; i++) {
            fprintf(stderr, "  [%d] %s\n", i, [devices[i].localizedName UTF8String]);
        }
    }

    AVCaptureDevice *device = nil;
    NSString *name = deviceName ? [NSString stringWithUTF8String:deviceName] : @"";

    // Try name matching first
    if (name.length > 0) {
        for (AVCaptureDevice *d in devices) {
            if ([d.localizedName rangeOfString:name options:NSCaseInsensitiveSearch].location != NSNotFound) {
                device = d;
                break;
            }
        }
    }

    // Fallback: numeric index
    if (!device) {
        int idx = [name intValue];
        if (idx >= 0 && idx < (int)devices.count) {
            device = devices[idx];
        }
    }
    // Fallback: default device
    if (!device) {
        device = [AVCaptureDevice defaultDeviceWithMediaType:AVMediaTypeVideo];
    }

    if (!device) {
        fprintf(stderr, "capture: no device found\n");
        return NULL;
    }
    fprintf(stderr, "capture: selected: %s\n", [device.localizedName UTF8String]);

    GoKVMCaptureSession *session = [[GoKVMCaptureSession alloc] init];
    if (!session) return NULL;

    NSError *err = nil;
    AVCaptureDeviceInput *input = [AVCaptureDeviceInput deviceInputWithDevice:device error:&err];
    if (!input) {
        fprintf(stderr, "capture: cannot create device input: %s\n", [err.localizedDescription UTF8String] ?: "unknown");
        [session release];
        return NULL;
    }
    if (![session canAddInput:input]) {
        fprintf(stderr, "capture: cannot add device input\n");
        [session release];
        return NULL;
    }
    [session addInput:input];

    AVCaptureVideoDataOutput *output = [[AVCaptureVideoDataOutput alloc] init];
    output.alwaysDiscardsLateVideoFrames = YES;
    // Ordinary cameras retain their current output policy. USB screen devices
    // require decoded pixels for the host encoder, not compressed muxed data.

    GoKVMCaptureDelegate *delegate = [[GoKVMCaptureDelegate alloc] init];
    session.frameDelegate = delegate;
    [delegate release];
    dispatch_queue_t queue = dispatch_queue_create("gokvm.capture", DISPATCH_QUEUE_SERIAL);
    [output setSampleBufferDelegate:delegate queue:queue];
    dispatch_release(queue);

    if (![session canAddOutput:output]) {
        fprintf(stderr, "capture: cannot add video output\n");
        [output setSampleBufferDelegate:nil queue:NULL];
        [output release];
        [session release];
        return NULL;
    }
    [session addOutput:output];

    if ([device hasMediaType:AVMediaTypeMuxed]) {
        AVCaptureAudioDataOutput *audio=[[AVCaptureAudioDataOutput alloc] init];
        audio.audioSettings=@{AVFormatIDKey:@(kAudioFormatLinearPCM), AVSampleRateKey:@48000,
            AVNumberOfChannelsKey:@2, AVLinearPCMBitDepthKey:@16, AVLinearPCMIsFloatKey:@NO,
            AVLinearPCMIsBigEndianKey:@NO, AVLinearPCMIsNonInterleaved:@NO};
        if([session canAddOutput:audio]) {
            [session addOutput:audio];
            AVCaptureConnection *connection=[audio connectionWithMediaType:AVMediaTypeAudio];
            if(connection) {
                BKUSBAudio *tap=[[BKUSBAudio alloc] init];session.audioDelegate=tap;
                dispatch_queue_t aq=dispatch_queue_create("anykvm.usb.audio",DISPATCH_QUEUE_SERIAL);
                [audio setSampleBufferDelegate:tap queue:aq];dispatch_release(aq);[tap release];
                connection.enabled=YES;
                fprintf(stderr,"capture: iOS USB audio output connected (48kHz stereo PCM)\n");
            } else { [session removeOutput:audio];fprintf(stderr,"capture: USB device has no audio port\n"); }
        }
        [audio release];
    }

    if ([device hasMediaType:AVMediaTypeMuxed]) {
        NSArray<NSNumber *> *formats = output.availableVideoCVPixelFormatTypes;
        OSType chosen = 0;
        OSType preferred[] = {kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange,
                              kCVPixelFormatType_420YpCbCr8BiPlanarFullRange,
                              kCVPixelFormatType_32BGRA};
        for (int i = 0; i < 3; i++) {
            if ([formats containsObject:@(preferred[i])]) { chosen = preferred[i]; break; }
        }
        output.videoSettings = chosen ? @{(id)kCVPixelBufferPixelFormatTypeKey: @(chosen)} : nil;
        fprintf(stderr, "capture: iOS decoded video output pixel_format=%u supported=%s\n",
                (unsigned)chosen, [[formats description] UTF8String]);
    }

    // Configure format for MJPEG + target FPS + resolution
    NSError *cfgErr = nil;
    BOOL holdConfiguration = NO;
    if (![device lockForConfiguration:&cfgErr]) {
        fprintf(stderr, "capture: cannot lock device for config: %s\n", [cfgErr.localizedDescription UTF8String]);
        if(pixel_format) { [output setSampleBufferDelegate:nil queue:NULL];[output release];[session release];return NULL; }
    } else {
        holdConfiguration = pixel_format != 0;
        // Log all available formats for debugging
        fprintf(stderr, "capture: available formats:\n");
        for (AVCaptureDeviceFormat *fmt in device.formats) {
            CMFormatDescriptionRef desc = fmt.formatDescription;
            if (CMFormatDescriptionGetMediaType(desc) != kCMMediaType_Video) continue;
            CMVideoDimensions dims = CMVideoFormatDescriptionGetDimensions(desc);
            FourCharCode codec = CMFormatDescriptionGetMediaSubType(desc);
            char c[5] = {(char)(codec>>24),(char)(codec>>16),(char)(codec>>8),(char)codec,0};
            float maxFPS = 0;
            for (AVFrameRateRange *rng in fmt.videoSupportedFrameRateRanges) {
                if (rng.maxFrameRate > maxFPS) maxFPS = rng.maxFrameRate;
            }
            fprintf(stderr, "  %dx%d codec=%s maxFPS=%.0f\n", dims.width, dims.height, c, maxFPS);
        }

        AVCaptureDeviceFormat *bestFormat = nil;
        AVFrameRateRange *bestRange = nil;
        if (pixel_format) {
            for (AVCaptureDeviceFormat *fmt in device.formats) {
                CMFormatDescriptionRef desc=fmt.formatDescription;
                if(CMFormatDescriptionGetMediaType(desc)!=kCMMediaType_Video)continue;
                FourCharCode code=CMFormatDescriptionGetMediaSubType(desc);
                BOOL jpegAlias=pixel_format=='jpeg' && (code=='dmb1'||code=='mjpg');
                if(code!=pixel_format && !jpegAlias)continue;
                CMVideoDimensions dims=CMVideoFormatDescriptionGetDimensions(desc);
                if((width>0&&dims.width!=width)||(height>0&&dims.height!=height))continue;
                for(AVFrameRateRange *range in fmt.videoSupportedFrameRateRanges) {
                    if(fps<=0 || (fps>=range.minFrameRate-0.001 && fps<=range.maxFrameRate+0.001)) {
                        bestFormat=fmt;bestRange=range;goto format_done;
                    }
                }
            }
            fprintf(stderr,"capture: requested input format/mode is unsupported\n");
            [device unlockForConfiguration];[output setSampleBufferDelegate:nil queue:NULL];
            [output release];[session release];return NULL;
        }
        for (AVCaptureDeviceFormat *fmt in device.formats) {
            CMFormatDescriptionRef desc = fmt.formatDescription;
            if (CMFormatDescriptionGetMediaType(desc) != kCMMediaType_Video) continue;
            FourCharCode codec = CMFormatDescriptionGetMediaSubType(desc);
            if (codec != 'jpeg' && codec != 'dmb1' && codec != 'mjpg') continue;

            CMVideoDimensions dims = CMVideoFormatDescriptionGetDimensions(desc);
            if (width > 0 && dims.width != width) continue;
            if (height > 0 && dims.height != height) continue;

            for (AVFrameRateRange *rng in fmt.videoSupportedFrameRateRanges) {
                if ((int)rng.maxFrameRate >= fps) {
                    bestFormat = fmt;
                    bestRange = rng;
                    goto format_done;
                }
            }
        }
        // If no exact match, fallback to any MJPEG format
        if (!bestFormat) {
            for (AVCaptureDeviceFormat *fmt in device.formats) {
                CMFormatDescriptionRef desc = fmt.formatDescription;
                if (CMFormatDescriptionGetMediaType(desc) != kCMMediaType_Video) continue;
                FourCharCode codec = CMFormatDescriptionGetMediaSubType(desc);
                if (codec != 'jpeg' && codec != 'dmb1' && codec != 'mjpg') continue;
                for (AVFrameRateRange *rng in fmt.videoSupportedFrameRateRanges) {
                    if ((int)rng.maxFrameRate >= fps) {
                        bestFormat = fmt;
                        bestRange = rng;
                        goto format_done;
                    }
                }
            }
        }
        // If no MJPEG match, try any format with requested resolution
        if (!bestFormat && (width > 0 || height > 0)) {
            for (AVCaptureDeviceFormat *fmt in device.formats) {
                CMFormatDescriptionRef desc = fmt.formatDescription;
                if (CMFormatDescriptionGetMediaType(desc) != kCMMediaType_Video) continue;
                CMVideoDimensions dims = CMVideoFormatDescriptionGetDimensions(desc);
                if (width > 0 && dims.width != width) continue;
                if (height > 0 && dims.height != height) continue;
                for (AVFrameRateRange *rng in fmt.videoSupportedFrameRateRanges) {
                    if ((int)rng.maxFrameRate >= fps) {
                        bestFormat = fmt;
                        bestRange = rng;
                        goto format_done;
                    }
                }
            }
        }
    format_done:
        if (bestFormat) {
            device.activeFormat = bestFormat;
            if (bestRange && fps > 0) {
                double effectiveFPS=fmin(bestRange.maxFrameRate,fmax(bestRange.minFrameRate,fps));
                CMTime duration = fabs(effectiveFPS-bestRange.maxFrameRate)<0.001 ? bestRange.minFrameDuration :
                    (fabs(effectiveFPS-bestRange.minFrameRate)<0.001 ? bestRange.maxFrameDuration : CMTimeMakeWithSeconds(1.0/effectiveFPS,1000000000));
                device.activeVideoMinFrameDuration = duration;
                device.activeVideoMaxFrameDuration = duration;
            }
            if (pixel_format && ![device hasMediaType:AVMediaTypeMuxed]) {
                FourCharCode native=CMFormatDescriptionGetMediaSubType(bestFormat.formatDescription);
                if ([output.availableVideoCVPixelFormatTypes containsObject:@(native)]) {
                    output.videoSettings=@{(id)kCVPixelBufferPixelFormatTypeKey:@(native)};
                }
            }
            CMFormatDescriptionRef desc = bestFormat.formatDescription;
            CMVideoDimensions dims = CMVideoFormatDescriptionGetDimensions(desc);
            FourCharCode codec = CMFormatDescriptionGetMediaSubType(desc);
            char codecStr[5] = {(char)(codec>>24),(char)(codec>>16),(char)(codec>>8),(char)codec,0};
            fprintf(stderr, "capture: format %dx%d maxFPS=%.0f codec=%s\n", dims.width, dims.height,
                    bestRange ? bestRange.maxFrameRate : 0.0, codecStr);
        } else {
            fprintf(stderr, "capture: no MJPEG format found, using default\n");
        }
        if(!holdConfiguration)[device unlockForConfiguration];
    }

    // Reopen the queue for EVERY real session. DAL discovery runs only once,
    // whereas shutdown marks this queue closed on every restart/reconnect.
    frame_buf_reset();
    [session startRunning];
    if(holdConfiguration)[device unlockForConfiguration];
    CMVideoDimensions activeDims=CMVideoFormatDescriptionGetDimensions(device.activeFormat.formatDescription);
    FourCharCode activeCode=CMFormatDescriptionGetMediaSubType(device.activeFormat.formatDescription);
    fprintf(stderr,"capture: active input %dx%d format=%c%c%c%c fps=%.3f\n",activeDims.width,activeDims.height,
        (char)(activeCode>>24),(char)(activeCode>>16),(char)(activeCode>>8),(char)activeCode,
        1.0/CMTimeGetSeconds(device.activeVideoMinFrameDuration));
    BOOL jpegAlias=pixel_format=='jpeg' && (activeCode=='dmb1'||activeCode=='mjpg');
    double activeFPS=1.0/CMTimeGetSeconds(device.activeVideoMinFrameDuration);
    if(pixel_format && ((width>0 && activeDims.width!=width) || (height>0 && activeDims.height!=height) ||
        (activeCode!=pixel_format && !jpegAlias) || (fps>0 && (!isfinite(activeFPS)||fabs(activeFPS-fps)>0.001)))) {
        fprintf(stderr,"capture: session did not retain the requested input mode\n");
        [output release];avfoundation_stop((void *)session);return NULL;
    }
    AVCaptureConnection *videoConnection = [output connectionWithMediaType:AVMediaTypeVideo];
    fprintf(stderr, "capture: session started running=%d video_connection=%d enabled=%d active=%d ports=%lu\n",
            session.isRunning, videoConnection != nil,
            videoConnection.isEnabled, videoConnection.isActive, (unsigned long)input.ports.count);

    [output release];

    return (void *)session; // Transfer the alloc retain to avfoundation_stop.
}

// Preserve the device's rational frame duration instead of rounding its rate.
void avfoundation_rate(void *session, int *num, int *den) {
    *num = 0; *den = 0;
    AVCaptureSession *s = (__bridge AVCaptureSession *)session;
    for (AVCaptureInput *input in s.inputs) {
        if (![input isKindOfClass:[AVCaptureDeviceInput class]]) continue;
        CMTime duration = ((AVCaptureDeviceInput *)input).device.activeVideoMinFrameDuration;
        if (CMTIME_IS_NUMERIC(duration) && duration.value > 0 && duration.value <= INT32_MAX && duration.timescale > 0) {
            *num = duration.timescale; *den = (int)duration.value;
            return;
        }
    }
}

void avfoundation_stop(void *session) {
    if (session) {
        AVCaptureSession *s = (AVCaptureSession *)session;
        [s stopRunning];
        BKUSBAudio *tap=((GoKVMCaptureSession *)s).audioDelegate;
        if(tap) {pthread_mutex_lock(&tap->mutex);tap->closed=YES;tap->used=0;pthread_mutex_unlock(&tap->mutex);}
        for (AVCaptureOutput *output in s.outputs) {
            if ([output isKindOfClass:[AVCaptureAudioDataOutput class]]) {
                AVCaptureAudioDataOutput *audio=(AVCaptureAudioDataOutput *)output;
                dispatch_queue_t queue=audio.sampleBufferCallbackQueue;
                if(queue)dispatch_retain(queue);
                [audio setSampleBufferDelegate:nil queue:NULL];
                if(queue){dispatch_sync(queue,^{});dispatch_release(queue);}
                continue;
            }
            if (![output isKindOfClass:[AVCaptureVideoDataOutput class]]) continue;
            AVCaptureVideoDataOutput *video = (AVCaptureVideoDataOutput *)output;
            dispatch_queue_t queue = video.sampleBufferCallbackQueue;
            if (queue) dispatch_retain(queue);
            [video setSampleBufferDelegate:nil queue:NULL];
            if (queue) { dispatch_sync(queue, ^{}); dispatch_release(queue); }
        }
        frame_buf_destroy();
        [s release];
    }
}

void *capture_usb_audio_open(void *session) {
    BKUSBAudio *tap=((GoKVMCaptureSession *)session).audioDelegate;
    if(!tap)return NULL;
    pthread_mutex_lock(&tap->mutex);
    if(tap->closed || tap->subscribed){pthread_mutex_unlock(&tap->mutex);return NULL;}
    tap->subscribed=YES;tap->used=0;[tap retain];pthread_mutex_unlock(&tap->mutex);return tap;
}
int capture_usb_audio_read(void *handle,uint8_t *out,int capacity) {
    BKUSBAudio *tap=(BKUSBAudio *)handle;
    pthread_mutex_lock(&tap->mutex);
    int n=tap->closed?-1:(int)MIN(tap->used,(size_t)(capacity&~3));
    if(n>0){memcpy(out,tap->pcm,n);tap->used-=n;memmove(tap->pcm,tap->pcm+n,tap->used);}
    pthread_mutex_unlock(&tap->mutex);return n;
}
void capture_usb_audio_close(void *handle) {
    BKUSBAudio *tap=(BKUSBAudio *)handle;
    pthread_mutex_lock(&tap->mutex);tap->subscribed=NO;tap->used=0;pthread_mutex_unlock(&tap->mutex);[tap release];
}

// avfoundation_find_device selects a device by name/index/default, matching the
// logic used by avfoundation_start. Returns a retained device or nil.
static AVCaptureDevice* avfoundation_find_device(const char *deviceName) {
    NSArray<AVCaptureDevice *> *devices = enumerate_devices();
    NSString *name=deviceName?[NSString stringWithUTF8String:deviceName]:@"";
    for(AVCaptureDevice *device in devices) if([device.localizedName isEqualToString:name])return device;
    for(AVCaptureDevice *device in devices) if(name.length && [device.localizedName rangeOfString:name options:NSCaseInsensitiveSearch].location!=NSNotFound)return device;
    NSScanner *scanner=[NSScanner scannerWithString:name];int index=-1;
    if([scanner scanInt:&index] && scanner.isAtEnd && index>=0 && index<(int)devices.count)return devices[index];
    if(!name.length)return [AVCaptureDevice defaultDeviceWithMediaType:AVMediaTypeVideo];
    return nil;
}

// Returns owned JSON metadata without opening or reconfiguring the device.
char *avfoundation_modes(const char *deviceName) {
    @autoreleasepool {
        AVCaptureDevice *device=avfoundation_find_device(deviceName);
        if(!device)return NULL;
        NSMutableArray *modes=[NSMutableArray array];
        for(AVCaptureDeviceFormat *format in device.formats) {
            CMFormatDescriptionRef desc=format.formatDescription;
            if(CMFormatDescriptionGetMediaType(desc)!=kCMMediaType_Video)continue;
            CMVideoDimensions dims=CMVideoFormatDescriptionGetDimensions(desc);
            for(AVFrameRateRange *rate in format.videoSupportedFrameRateRanges) {
                [modes addObject:@{@"Width":@(dims.width),@"Height":@(dims.height),
                    @"Subtype":@(CMFormatDescriptionGetMediaSubType(desc)),
                    @"MinFPS":@(rate.minFrameRate),@"MaxFPS":@(rate.maxFrameRate)}];
            }
        }
        NSData *json=[NSJSONSerialization dataWithJSONObject:modes options:0 error:NULL];
        if(!json)return NULL;
        char *result=malloc(json.length+1);if(!result)return NULL;
        memcpy(result,json.bytes,json.length);result[json.length]=0;return result;
    }
}

// avfoundation_probe enumerates the device's supported formats WITHOUT starting
// a capture session (no camera-permission prompt, does not hold the device),
// and picks the best format:
//   - Prefer an exact match of the requested width/height (want_w/want_h).
//   - Prefer MJPEG when available (better for JPEG clients).
//   - Otherwise pick the highest-resolution format.
// Fills out_w/out_h/out_fps with the chosen mode and out_mjpeg (1/0) with
// whether that mode is MJPEG. Returns 1 on success, 0 if no device/formats.
int avfoundation_probe(const char *deviceName, int want_w, int want_h, int want_fps,
                       int *out_w, int *out_h, int *out_fps, int *out_mjpeg) {
    @autoreleasepool {
        AVCaptureDevice *device = avfoundation_find_device(deviceName);
        if (!device) {
            return 0;
        }

        AVCaptureDeviceFormat *best = nil;
        int bestScore = -1;
        int bestFPS = 0;
        int bestMJPEG = 0;

        for (AVCaptureDeviceFormat *fmt in device.formats) {
            CMFormatDescriptionRef desc = fmt.formatDescription;
            if (CMFormatDescriptionGetMediaType(desc) != kCMMediaType_Video) continue;
            CMVideoDimensions dims = CMVideoFormatDescriptionGetDimensions(desc);
            FourCharCode codec = CMFormatDescriptionGetMediaSubType(desc);
            int isMJPEG = (codec == 'jpeg' || codec == 'dmb1' || codec == 'mjpg') ? 1 : 0;

            int maxFPS = 0;
            for (AVFrameRateRange *rng in fmt.videoSupportedFrameRateRanges) {
                if ((int)rng.maxFrameRate > maxFPS) maxFPS = (int)rng.maxFrameRate;
            }

            // Scoring: exact resolution match dominates, then MJPEG, then area.
            int score = 0;
            if (want_w > 0 && want_h > 0 && dims.width == want_w && dims.height == want_h) {
                score += 1000000000;
            }
            if (isMJPEG) {
                score += 100000000;
            }
            score += dims.width * dims.height / 100; // area tiebreaker (scaled)

            if (score > bestScore) {
                bestScore = score;
                best = fmt;
                bestFPS = maxFPS;
                bestMJPEG = isMJPEG;
            }
        }

        if (!best) {
            return 0;
        }

        CMVideoDimensions dims = CMVideoFormatDescriptionGetDimensions(best.formatDescription);
        int chosenFPS = bestFPS;
        if (want_fps > 0 && want_fps <= bestFPS) {
            chosenFPS = want_fps;
        }
        if (chosenFPS <= 0) chosenFPS = 30;

        if (out_w) *out_w = dims.width;
        if (out_h) *out_h = dims.height;
        if (out_fps) *out_fps = chosenFPS;
        if (out_mjpeg) *out_mjpeg = bestMJPEG;
        return 1;
    }
}

// avfoundation_list_video_devices writes the list of available video capture
// devices (name and index) into buf (maxLen bytes). Each device is written
// as "idx\0name\n" so the Go side can split on newlines and parse.
// include_ios: when non-zero, warms DAL and waits for iOS mirrors (same as -video avf).
void avfoundation_list_video_devices(char *buf, int maxLen, int include_ios) {
    @autoreleasepool {
        NSArray<AVCaptureDevice *> *devices;
        if (include_ios) {
            devices = discover_video_devices_for_list();
        } else {
            devices = enumerate_devices();
        }

        int pos = 0;
        for (int i = 0; i < (int)devices.count && pos + 256 < maxLen; i++) {
            AVCaptureDevice *d = devices[i];
            const char *name = [d.localizedName UTF8String];
            pos += snprintf(buf + pos, maxLen - pos, "%d\t%s\n", i, name);
        }
        if (pos == 0) buf[0] = '\0';
    }
}
