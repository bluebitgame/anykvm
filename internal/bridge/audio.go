package bridge

import (
	"anykvm/internal/audiodec"
	"anykvm/internal/capture"
	"bytes"
	"encoding/binary"
	"math"
)

// EncodedAudioSink accepts a compressed format only after client negotiation.
type EncodedAudioSink interface{ SupportsEncodedAudio(capture.AudioFrame) bool }

func setAudioAvailable(sink AudioSink, available bool) {
	if status, ok := sink.(interface{ SetAudioAvailable(bool) }); ok {
		status.SetAudioAvailable(available)
	}
}

type audioForwarder struct {
	decoder    audiodec.Decoder
	format     capture.AudioFrame
	newDecoder func(capture.AudioFrame) (audiodec.Decoder, error)
}

func (f *audioForwarder) close() {
	if f.decoder != nil {
		f.decoder.Close()
		f.decoder = nil
	}
}
func (f *audioForwarder) push(sink AudioSink, a capture.AudioFrame) error {
	if len(a.Encoded) == 0 {
		f.close()
		if a.Gain != nil {
			a.PCM = bytes.Clone(a.PCM)
			applyAudioGain(a.PCM, *a.Gain)
			a.Gain = nil
		}
		setAudioAvailable(sink, true)
		sink.PushAudio(a)
		return nil
	}
	if encoded, ok := sink.(EncodedAudioSink); ok && encoded.SupportsEncodedAudio(a) {
		f.close()
		setAudioAvailable(sink, true)
		sink.PushAudio(a)
		return nil
	}
	if f.format.Codec != a.Codec || f.format.SampleRate != a.SampleRate || f.format.Channels != a.Channels || f.format.FramesPerPacket != a.FramesPerPacket || f.format.Epoch != a.Epoch || !bytes.Equal(f.format.Config, a.Config) {
		f.close()
	}
	if f.decoder == nil {
		factory := f.newDecoder
		if factory == nil {
			factory = audiodec.New
		}
		d, err := factory(a)
		if err != nil {
			return err
		}
		f.decoder = d
		f.format = a
		f.format.Config = bytes.Clone(a.Config)
		f.format.Encoded = nil
		f.format.PCM = nil
	}
	pcm, err := f.decoder.Decode(a.Encoded)
	if err != nil {
		f.close() // A corrupt AU must not leave AudioToolbox permanently stuck.
		return err
	}
	if len(pcm) > 0 {
		if a.Gain != nil {
			applyAudioGain(pcm, *a.Gain)
		}
		setAudioAvailable(sink, true)
		sink.PushAudio(capture.AudioFrame{PCM: pcm, SampleRate: 48000, Channels: 2, Timestamp: a.Timestamp})
	}
	return nil
}

func applyAudioGain(pcm []byte, gain float64) {
	if math.IsNaN(gain) || math.IsInf(gain, 0) {
		gain = 1
	}
	gain = max(0, min(1, gain))
	if gain == 1 {
		return
	}
	for i := 0; i+1 < len(pcm); i += 2 {
		v := int16(binary.LittleEndian.Uint16(pcm[i:]))
		binary.LittleEndian.PutUint16(pcm[i:], uint16(int16(math.Round(float64(v)*gain))))
	}
}
