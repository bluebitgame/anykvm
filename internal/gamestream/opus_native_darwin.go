//go:build darwin && cgo

package gamestream

/*
#cgo LDFLAGS: -framework AudioToolbox
#include <AudioToolbox/AudioToolbox.h>
#include <stdlib.h>

static AudioCodec anykvm_opus_open(int duration, int bitrate, OSStatus *result) {
 AudioComponentDescription desc={0};
 desc.componentType=kAudioEncoderComponentType;
 desc.componentSubType=kAudioFormatOpus;
 AudioComponent component=AudioComponentFindNext(NULL,&desc);
 if(!component) { *result=-1; return NULL; }
 AudioComponentInstance codec=NULL;
 *result=AudioComponentInstanceNew(component,&codec);
 if(*result) return NULL;
 AudioStreamBasicDescription in={0},out={0};
 in.mSampleRate=48000; in.mFormatID=kAudioFormatLinearPCM;
 in.mFormatFlags=kAudioFormatFlagIsFloat|kAudioFormatFlagIsPacked;
 in.mBytesPerPacket=8; in.mFramesPerPacket=1; in.mBytesPerFrame=8;
 in.mChannelsPerFrame=2; in.mBitsPerChannel=32;
 out.mSampleRate=48000; out.mFormatID=kAudioFormatOpus;
 out.mChannelsPerFrame=2; out.mFramesPerPacket=duration*48;
 UInt32 mode=kAudioCodecBitRateControlMode_Constant,rate=bitrate;
 *result=AudioCodecSetProperty(codec,kAudioCodecPropertyBitRateControlMode,sizeof(mode),&mode);
 if(!*result) *result=AudioCodecSetProperty(codec,kAudioCodecPropertyCurrentTargetBitRate,sizeof(rate),&rate);
 if(!*result) *result=AudioCodecInitialize(codec,&in,&out,NULL,0);
 UInt32 frames=0,size=sizeof(frames);
 if(!*result) *result=AudioCodecGetProperty(codec,kAudioCodecPropertyPacketFrameSize,&size,&frames);
 if(!*result && frames!=out.mFramesPerPacket) *result=-2;
 if(*result) { AudioComponentInstanceDispose(codec); return NULL; }
 return codec;
}
static OSStatus anykvm_opus_encode(AudioCodec codec, const void *pcm, UInt32 pcmSize, void *out, UInt32 *size) {
 if(pcmSize>1920 || pcmSize%4) return -3;
 float samples[960];const unsigned char *input=pcm;
 for(UInt32 i=0;i<pcmSize/2;i++){int16_t value=(int16_t)((uint16_t)input[i*2]|((uint16_t)input[i*2+1]<<8));samples[i]=value/32768.0f;}
 UInt32 bytes=pcmSize*2,frames=pcmSize/4;
 OSStatus s=AudioCodecAppendInputData(codec,samples,&bytes,&frames,NULL);
 if(s) return s;
 if(bytes!=pcmSize*2) return -3;
 UInt32 packets=1,status=0;
 AudioStreamPacketDescription packet={0};
 s=AudioCodecProduceOutputPackets(codec,out,size,&packets,&packet,&status);
 if(s) return s;
 if(status==kAudioCodecProduceOutputPacketFailure) return -4;
 if(packets==0) *size=0;
 return 0;
}
static void anykvm_opus_close(AudioCodec codec) {
 AudioCodecUninitialize(codec); AudioComponentInstanceDispose(codec);
}
*/
import "C"
import (
	"fmt"
	"sync"
	"unsafe"
)

type appleOpusEncoder struct {
	mu    sync.Mutex
	codec C.AudioCodec
	bytes int
}

func newNativeOpusEncoder(duration int) (nativeOpusEncoder, error) {
	if duration != 5 && duration != 10 {
		return nil, fmt.Errorf("unsupported Opus duration %d", duration)
	}
	var status C.OSStatus
	codec := C.anykvm_opus_open(C.int(duration), C.int(opusBitrateKbps*1000), &status)
	if codec == nil {
		return nil, fmt.Errorf("AudioToolbox Opus initialization failed: %d", int(status))
	}
	return &appleOpusEncoder{codec: codec, bytes: duration * 48 * 4}, nil
}
func (e *appleOpusEncoder) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.codec != nil {
		C.anykvm_opus_close(e.codec)
		e.codec = nil
	}
}
func (e *appleOpusEncoder) Encode(pcm []byte) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.codec == nil || len(pcm) != e.bytes {
		return nil, fmt.Errorf("invalid Opus encoder/input")
	}
	out := make([]byte, 4096)
	size := C.UInt32(len(out))
	status := C.anykvm_opus_encode(e.codec, unsafe.Pointer(&pcm[0]), C.UInt32(len(pcm)), unsafe.Pointer(&out[0]), &size)
	if status != 0 {
		return nil, fmt.Errorf("AudioToolbox Opus encode failed: %d", int(status))
	}
	return out[:int(size)], nil
}
