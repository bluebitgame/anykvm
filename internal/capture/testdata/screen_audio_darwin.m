#import "../screen_darwin.m"
#include <assert.h>

// Synthetic CoreMedia samples only: no display/audio capture or permissions.
static void check(BOOL planar) {
 BKScreen *s = [BKScreen new]; s->audioEnabled = YES;
 AudioStreamBasicDescription f = {0};
 f.mSampleRate = 48000; f.mFormatID = kAudioFormatLinearPCM;
 f.mFormatFlags = kAudioFormatFlagIsFloat | kAudioFormatFlagIsPacked | (planar ? kAudioFormatFlagIsNonInterleaved : 0);
 f.mChannelsPerFrame = 2; f.mBitsPerChannel = 32; f.mFramesPerPacket = 1;
 f.mBytesPerFrame = f.mBytesPerPacket = planar ? 4 : 8;
 CMAudioFormatDescriptionRef desc = NULL;
 assert(CMAudioFormatDescriptionCreate(NULL, &f, 0, NULL, 0, NULL, NULL, &desc) == noErr);
 float interleaved[] = {.25f, -.25f, .5f, -.5f};
 float planes[] = {.25f, .5f, -.25f, -.5f};
 CMBlockBufferRef block = NULL;
 assert(CMBlockBufferCreateWithMemoryBlock(NULL, NULL, sizeof(planes), NULL, NULL, 0, sizeof(planes), 0, &block) == noErr);
 assert(CMBlockBufferReplaceDataBytes(planar ? planes : interleaved, block, 0, sizeof(planes)) == noErr);
 CMSampleBufferRef sample = NULL;
 assert(CMAudioSampleBufferCreateReadyWithPacketDescriptions(NULL, block, desc, 2, kCMTimeZero, NULL, &sample) == noErr);
 bk_screen_buffer_audio(s, sample);
 float out[4] = {0}; char error[512];
 assert(bk_screen_audio_read(s, out, sizeof(out), error, sizeof(error)) == sizeof(out));
 assert(memcmp(out, interleaved, sizeof(out)) == 0);
 assert(bk_screen_audio_read(s, out, sizeof(out), error, sizeof(error)) == 0);
 for (int i = 0; i < 20000; i++) bk_screen_buffer_audio(s, sample);
 assert(s->audio.length <= 38400 && s->audio.length % 8 == 0);
 CFRelease(sample); CFRelease(block); CFRelease(desc);
 [s shutdown];
 assert(bk_screen_audio_read(s, out, sizeof(out), error, sizeof(error)) < 0);
 [s release];
}
int main(void) { @autoreleasepool { check(NO); check(YES); puts("native planar/interleaved PCM and bounded queue passed"); } return 0; }
