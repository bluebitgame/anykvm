#import <AVFoundation/AVFoundation.h>
#import <CoreMedia/CoreMedia.h>
#include <assert.h>

extern void *capture_usb_audio_open(void *);
extern int capture_usb_audio_read(void *,uint8_t *,int);
extern void capture_usb_audio_close(void *);
extern void avfoundation_stop(void *);

static CMSampleBufferRef sample(int value) {
    AudioStreamBasicDescription asbd={0};asbd.mSampleRate=48000;asbd.mFormatID=kAudioFormatLinearPCM;
    asbd.mFormatFlags=kAudioFormatFlagIsSignedInteger|kAudioFormatFlagIsPacked;
    asbd.mFramesPerPacket=1;asbd.mChannelsPerFrame=2;asbd.mBitsPerChannel=16;asbd.mBytesPerFrame=asbd.mBytesPerPacket=4;
    CMAudioFormatDescriptionRef format=NULL;
    assert(CMAudioFormatDescriptionCreate(NULL,&asbd,0,NULL,0,NULL,NULL,&format)==noErr);
    CMBlockBufferRef block=NULL;assert(CMBlockBufferCreateWithMemoryBlock(NULL,NULL,1920,NULL,NULL,0,1920,0,&block)==noErr);
    int16_t pcm[960];for(int i=0;i<960;i++)pcm[i]=value;
    assert(CMBlockBufferReplaceDataBytes(pcm,block,0,1920)==noErr);
    CMSampleTimingInfo timing={CMTimeMake(1,48000),kCMTimeZero,kCMTimeInvalid};CMSampleBufferRef s=NULL;
    assert(CMSampleBufferCreateReady(NULL,block,format,480,1,&timing,0,NULL,&s)==noErr);
    CFRelease(block);CFRelease(format);return s;
}
int main(void) {
    @autoreleasepool {
        id session=[[NSClassFromString(@"GoKVMCaptureSession") alloc] init];
        id delegate=[[NSClassFromString(@"BKUSBAudio") alloc] init];
        [session setValue:delegate forKey:@"audioDelegate"];[delegate release];
        void *tap=capture_usb_audio_open(session);assert(tap);
        assert(capture_usb_audio_open(session)==NULL); // prevent competing readers
        for(int value=0;value<20;value++) {
            CMSampleBufferRef s=sample(value);
            [(id<AVCaptureAudioDataOutputSampleBufferDelegate>)delegate captureOutput:nil didOutputSampleBuffer:s fromConnection:nil];CFRelease(s);
        }
        int16_t out[1920];int count=capture_usb_audio_read(tap,(uint8_t*)out,3840);
        assert(count==3840 && out[0]==10 && out[960]==11); // oldest 100ms was discarded
        capture_usb_audio_close(tap);
        tap=capture_usb_audio_open(session);assert(tap);assert(capture_usb_audio_read(tap,(uint8_t*)out,3840)==0);
        CMSampleBufferRef s=sample(1234);
        [(id<AVCaptureAudioDataOutputSampleBufferDelegate>)delegate captureOutput:nil didOutputSampleBuffer:s fromConnection:nil];CFRelease(s);
        assert(capture_usb_audio_read(tap,(uint8_t*)out,3840)==1920 && out[0]==1234);
        // The outstanding audio lease survives the destruction of its session.
        avfoundation_stop(session);
        assert(capture_usb_audio_read(tap,(uint8_t*)out,3840)==-1);
        capture_usb_audio_close(tap);
    }
    return 0;
}
