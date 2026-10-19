package audiodec

import (
	"anykvm/internal/capture"
	"bytes"
	"testing"
)

func TestALACCookieValidation(t *testing.T) {
	f := capture.AudioFrame{Codec: "alac", SampleRate: 44100, Channels: 2, FramesPerPacket: 352,
		Config: []byte{0, 0, 1, 0x60, 0, 16, 40, 10, 14, 2, 0, 255, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xac, 0x44}}
	cookie, err := alacMagicCookie(f)
	if err != nil || !bytes.Equal(cookie, f.Config) {
		t.Fatal("ALAC cookie", cookie, err)
	}
	for _, change := range []func(*capture.AudioFrame){
		func(f *capture.AudioFrame) { f.Config = f.Config[:23] },
		func(f *capture.AudioFrame) { f.Config[4] = 1 },
		func(f *capture.AudioFrame) { f.Config[5] = 24 },
		func(f *capture.AudioFrame) { f.SampleRate = 48000 },
		func(f *capture.AudioFrame) { f.Channels = 1 },
		func(f *capture.AudioFrame) { f.FramesPerPacket = 4096 },
	} {
		invalid := f
		invalid.Config = bytes.Clone(f.Config)
		change(&invalid)
		if _, err := alacMagicCookie(invalid); err == nil {
			t.Fatal("accepted mismatched ALAC format", invalid)
		}
	}
}
