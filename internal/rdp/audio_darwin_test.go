//go:build darwin && cgo

package rdp

import (
	"bytes"
	"encoding/binary"
	"os"
	"sync"
	"testing"

	"anykvm/internal/avmedia"
	"anykvm/internal/bridge"
	"anykvm/internal/capture"
)

// Synthetic AAC-ELD from the AirPlay fixture goes through the real Bridge
// decoder and RDP sender, without an audio encoder or media-library backend.
type rdpCompressedSource struct {
	stop   chan struct{}
	frames chan capture.AudioFrame
	closed sync.Once
}

func (d *rdpCompressedSource) Open() error                        { return nil }
func (d *rdpCompressedSource) OpenWithFormat(int, int, int) error { return nil }
func (d *rdpCompressedSource) Capture() (*capture.Frame, error) {
	<-d.stop
	return nil, capture.ErrStreamStopped
}
func (d *rdpCompressedSource) Close() error      { d.closed.Do(func() { close(d.stop) }); return nil }
func (*rdpCompressedSource) SetDecodeImage(bool) {}
func (*rdpCompressedSource) SetJPEGQuality(int)  {}

type rdpCompressedAudio struct{ source *rdpCompressedSource }

func (*rdpCompressedAudio) Open() error  { return nil }
func (*rdpCompressedAudio) Close() error { return nil }
func (d *rdpCompressedAudio) Capture() (*capture.AudioFrame, error) {
	select {
	case f := <-d.source.frames:
		return &f, nil
	case <-d.source.stop:
		return nil, capture.ErrStreamStopped
	}
}

func TestSystemAACELDToRDPPCM(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_SYSTEM_AUDIO") == "" {
		t.Skip("set ANYKVM_TEST_SYSTEM_AUDIO to enable system codec checks")
	}
	if os.Getenv("ANYKVM_TEST_WITHOUT_FFMPEG") != "" && avmedia.Available() == nil {
		t.Fatal("FFmpeg is available; this test must run without it")
	}
	data, err := os.ReadFile("../testdata/tone.aaceld")
	if err != nil {
		t.Fatal(err)
	}
	c, conn := audioTestClient(t)
	negotiateTestAudio(t, c, conn, 8)
	c.startAudioLoop()
	source := &rdpCompressedSource{stop: make(chan struct{}), frames: make(chan capture.AudioFrame)}
	b := bridge.NewBridge([]*bridge.MachineSession{{Capture: source, AudioDev: &rdpCompressedAudio{source}}}, "alt", 30, 0, 0)
	b.SetRDP(c.s)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	b.StartRDPWithCodec("clearcodec")
	// Pace the synthetic source using the client's confirmations so that the
	// bounded capture queue does not discard packets during this content check.
	var pcm []byte
	for len(data) >= 2 {
		n := int(binary.BigEndian.Uint16(data))
		data = data[2:]
		if n > len(data) {
			t.Fatal("truncated AAC fixture")
		}
		source.frames <- capture.AudioFrame{Encoded: bytes.Clone(data[:n]), Codec: "mp4a.40.39", Config: []byte{0xf8, 0xe8, 0x50, 0}, SampleRate: 44100, Channels: 2, FramesPerPacket: 480}
		data = data[n:]
		wave := readAudioMessage(t, conn)
		if wave[0] != soundWave2 || len(wave) < 16 || (len(wave)-16)%4 != 0 {
			t.Fatal("AAC-ELD did not become RDP stereo PCM")
		}
		pcm = append(pcm, wave[16:]...)
		if err := c.handleAudio(audioPDU(soundConfirm, join(wave[4:6], []byte{wave[8], 0}))); err != nil {
			t.Fatal(err)
		}
	}
	frames := len(pcm) / 4
	var previous [2]int16
	var crossings [2]int
	for i := 0; i+3 < len(pcm); i += 4 {
		for channel := 0; channel < 2; channel++ {
			sample := int16(le.Uint16(pcm[i+2*channel:]))
			if previous[channel] <= 0 && sample > 0 {
				crossings[channel]++
			}
			previous[channel] = sample
		}
	}
	if frames < 23500 || frames > 25000 || crossings[0] < 200 || crossings[0] > 240 || crossings[1] < 310 || crossings[1] > 360 {
		t.Fatalf("RDP PCM corrupted: frames=%d tone_cycles=%v", frames, crossings)
	}
	t.Logf("AirPlay AAC-ELD through system decoder to RDP: %d stereo frames, tone cycles %v", frames, crossings)
}
