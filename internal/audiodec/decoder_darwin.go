//go:build darwin && cgo

package audiodec

/*
#cgo LDFLAGS: -framework AudioToolbox
#include <AudioToolbox/AudioToolbox.h>

typedef struct {
 const void *data; UInt32 size, channels, frames;
 AudioStreamPacketDescription description;
} bk_audio_input;
static OSStatus bk_audio_read(AudioConverterRef c, UInt32 *packets,
 AudioBufferList *buffer, AudioStreamPacketDescription **description, void *opaque) {
 bk_audio_input *in=opaque;
 if (!in->size) { *packets=0; return 1; }
 *packets=1; buffer->mNumberBuffers=1;
 buffer->mBuffers[0]=(AudioBuffer){in->channels,in->size,(void*)in->data};
 in->description=(AudioStreamPacketDescription){0,in->frames,in->size};
 if(description) *description=&in->description;
 in->size=0; return noErr;
}
static OSStatus bk_audio_open(AudioConverterRef *c, int kind, double rate,
 UInt32 channels, UInt32 frames, const void *cookie, UInt32 cookieSize) {
 AudioStreamBasicDescription in={0},out={0};
 in.mSampleRate=rate;
 in.mFormatID=kind==2?kAudioFormatAppleLossless:kind==1?kAudioFormatMPEG4AAC_ELD:kAudioFormatMPEG4AAC;
 if(kind==2) in.mFormatFlags=kAppleLosslessFormatFlag_16BitSourceData;
 in.mFramesPerPacket=frames; in.mChannelsPerFrame=channels;
 out.mSampleRate=48000; out.mFormatID=kAudioFormatLinearPCM;
 out.mFormatFlags=kAudioFormatFlagIsSignedInteger|kAudioFormatFlagIsPacked;
 out.mFramesPerPacket=1; out.mChannelsPerFrame=2; out.mBitsPerChannel=16;
 out.mBytesPerFrame=out.mBytesPerPacket=4;
 OSStatus s=AudioConverterNew(&in,&out,c);
 if(!s && cookieSize) {
  s=AudioConverterSetProperty(*c,kAudioConverterDecompressionMagicCookie,cookieSize,cookie);
  if(s) { AudioConverterDispose(*c); *c=NULL; }
 }
 return s;
}
static OSStatus bk_audio_decode(AudioConverterRef c, const void *data, UInt32 size,
 UInt32 channels, UInt32 framesPerPacket, void *out, UInt32 *outSize) {
 bk_audio_input in={data,size,channels,framesPerPacket,{0}};
 AudioBufferList buffer={1,{{2,*outSize,out}}}; UInt32 frames=*outSize/4;
 OSStatus s=AudioConverterFillComplexBuffer(c,bk_audio_read,&in,&frames,&buffer,NULL);
 *outSize=buffer.mBuffers[0].mDataByteSize; return s==1?noErr:s;
}
*/
import "C"

import (
	"anykvm/internal/capture"
	"fmt"
	"io"
	"unsafe"
)

type nativeDecoder struct {
	converter        C.AudioConverterRef
	channels, frames C.UInt32
}

func newDecoder(f capture.AudioFrame) (Decoder, error) {
	kind := 0
	switch f.Codec {
	case "mp4a.40.39":
		kind = 1
	case "mp4a.40.2":
	case "alac":
		kind = 2
	default:
		return nil, fmt.Errorf("audio: unsupported codec %q", f.Codec)
	}
	if f.SampleRate <= 0 || f.Channels < 1 || f.Channels > 8 || len(f.Config) == 0 || f.FramesPerPacket <= 0 {
		return nil, fmt.Errorf("audio: invalid compressed format")
	}
	d := &nativeDecoder{channels: C.UInt32(f.Channels), frames: C.UInt32(f.FramesPerPacket)}
	// AAC needs MPEG-4 ES descriptors; ALAC uses its bare SpecificConfig.
	var cookie []byte
	var err error
	if kind == 2 {
		cookie, err = alacMagicCookie(f)
	} else {
		cookie, err = aacMagicCookie(f.Config)
	}
	if err != nil {
		return nil, err
	}
	if s := C.bk_audio_open(&d.converter, C.int(kind), C.double(f.SampleRate), d.channels, d.frames, unsafe.Pointer(&cookie[0]), C.UInt32(len(cookie))); s != 0 {
		return nil, fmt.Errorf("audio: AudioToolbox initialization: %d", int(s))
	}
	return d, nil
}
func (d *nativeDecoder) Decode(packet []byte) ([]byte, error) {
	if d.converter == nil {
		return nil, io.ErrClosedPipe
	}
	if len(packet) == 0 {
		return nil, nil
	}
	pcm := make([]byte, 16384)
	n := C.UInt32(len(pcm))
	if s := C.bk_audio_decode(d.converter, unsafe.Pointer(&packet[0]), C.UInt32(len(packet)), d.channels, d.frames, unsafe.Pointer(&pcm[0]), &n); s != 0 {
		return nil, fmt.Errorf("audio: AudioToolbox decode: %d", int(s))
	}
	return pcm[:int(n)], nil
}
func (d *nativeDecoder) Close() {
	if d.converter != nil {
		C.AudioConverterDispose(d.converter)
		d.converter = nil
	}
}
