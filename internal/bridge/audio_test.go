package bridge

import (
	"anykvm/internal/audiodec"
	"anykvm/internal/capture"
	"bytes"
	"encoding/binary"
	"sync"
	"testing"
	"time"
)

type audioSinkFixture struct {
	compressed bool
	frames     []capture.AudioFrame
}

func TestAudioSourceVolumeAndFlush(t *testing.T) {
	pcm := []byte{0x10, 0x27, 0xf0, 0xd8}
	applyAudioGain(pcm, 0.5)
	if int16(binary.LittleEndian.Uint16(pcm)) != 5000 || int16(binary.LittleEndian.Uint16(pcm[2:])) != -5000 {
		t.Fatal(pcm)
	}
	applyAudioGain(pcm, 0)
	if !bytes.Equal(pcm, make([]byte, 4)) {
		t.Fatal("mute failed")
	}
	var decoders []*audioDecoderFixture
	f := audioForwarder{newDecoder: func(capture.AudioFrame) (audiodec.Decoder, error) {
		d := &audioDecoderFixture{}
		decoders = append(decoders, d)
		return d, nil
	}}
	defer f.close()
	s := &audioSinkFixture{compressed: true}
	gain := 0.0
	a := capture.AudioFrame{Encoded: []byte{1, 2, 3}, Codec: "mp4a.40.39", SampleRate: 44100, Channels: 2, Config: []byte{0xf8, 0xe8, 0x50, 0}, FramesPerPacket: 480, Gain: &gain, Epoch: 1}
	if err := f.push(s, a); err != nil {
		t.Fatal(err)
	}
	if len(decoders) != 0 || !bytes.Equal(s.frames[0].Encoded, a.Encoded) || s.frames[0].Gain == nil || *s.frames[0].Gain != 0 {
		t.Fatal("passthrough lost mute or decoded")
	}
	s.compressed = false
	if err := f.push(s, a); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s.frames[1].PCM, make([]byte, 4)) {
		t.Fatal("fallback lost mute")
	}
	a.Epoch++
	if err := f.push(s, a); err != nil {
		t.Fatal(err)
	}
	if len(decoders) != 2 || decoders[0].closed != 1 {
		t.Fatal("FLUSH kept old decoder history")
	}
}

func (s *audioSinkFixture) SupportsEncodedAudio(capture.AudioFrame) bool { return s.compressed }
func (s *audioSinkFixture) PushAudio(f capture.AudioFrame)               { s.frames = append(s.frames, f) }

type audioDecoderFixture struct{ calls, closed int }

func (d *audioDecoderFixture) Decode(p []byte) ([]byte, error) {
	d.calls++
	return []byte{1, 2, 3, 4}, nil
}
func (d *audioDecoderFixture) Close() { d.closed++ }
func TestAudioPassthroughAndLazyFallback(t *testing.T) {
	d := &audioDecoderFixture{}
	opens := 0
	f := audioForwarder{newDecoder: func(capture.AudioFrame) (audiodec.Decoder, error) { opens++; return d, nil }}
	s := &audioSinkFixture{compressed: true}
	a := capture.AudioFrame{Encoded: []byte{10, 20, 30}, Codec: "mp4a.40.39", Config: []byte{0xf8, 0xe6, 0x50, 0}, SampleRate: 48000, Channels: 2, FramesPerPacket: 480}
	for i := 0; i < 3; i++ {
		if err := f.push(s, a); err != nil {
			t.Fatal(err)
		}
	}
	if opens != 0 || len(s.frames) != 3 || !bytes.Equal(s.frames[0].Encoded, a.Encoded) || len(s.frames[0].PCM) > 0 {
		t.Fatal("compressed path decoded or changed the payload")
	}
	s.compressed = false
	for i := 0; i < 2; i++ {
		if err := f.push(s, a); err != nil {
			t.Fatal(err)
		}
	}
	if opens != 1 || d.calls != 2 || !bytes.Equal(s.frames[4].PCM, []byte{1, 2, 3, 4}) || len(s.frames[4].Encoded) > 0 {
		t.Fatal("fallback did not reuse decoder or produce PCM")
	}
	s.compressed = true
	if err := f.push(s, a); err != nil {
		t.Fatal(err)
	}
	if d.closed != 1 || opens != 1 {
		t.Fatal("returning to direct audio kept a decoder alive")
	}
	if err := f.push(s, capture.AudioFrame{PCM: []byte{5, 6, 7, 8}, SampleRate: 48000, Channels: 2}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s.frames[6].PCM, []byte{5, 6, 7, 8}) || opens != 1 {
		t.Fatal("PCM did not pass through")
	}
	f.close()
}

type sessionAudioDevice struct {
	mu            sync.Mutex
	frames        chan capture.AudioFrame
	stop          chan struct{}
	opens, closes int
}

func (d *sessionAudioDevice) Open() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stop = make(chan struct{})
	d.opens++
	return nil
}
func (d *sessionAudioDevice) Capture() (*capture.AudioFrame, error) {
	d.mu.Lock()
	stop := d.stop
	d.mu.Unlock()
	select {
	case frame := <-d.frames:
		return &frame, nil
	case <-stop:
		// Some devices flush one last buffered frame when Close unblocks Capture.
		return &capture.AudioFrame{PCM: []byte{99, 0, 0, 0}, SampleRate: 48000, Channels: 2}, nil
	}
}
func (d *sessionAudioDevice) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closes++
	close(d.stop)
	return nil
}

type sessionAudioServer struct {
	takeoverServer
	pcm          chan capture.AudioFrame
	availability chan bool
}

func (s *sessionAudioServer) PushAudio(f capture.AudioFrame) { s.pcm <- f }
func (s *sessionAudioServer) SetAudioAvailable(v bool)       { s.availability <- v }

func TestRDPAudioRoutingSwitchAndDisconnect(t *testing.T) {
	first, second := &blockingCapture{}, &blockingCapture{}
	audio1 := &sessionAudioDevice{frames: make(chan capture.AudioFrame, 1)}
	audio2 := &sessionAudioDevice{frames: make(chan capture.AudioFrame, 1)}
	b := NewBridge([]*MachineSession{{Capture: first, AudioDev: audio1}, {Capture: second, AudioDev: audio2}}, "alt", 30, 0, 0)
	rdp := &sessionAudioServer{pcm: make(chan capture.AudioFrame, 4), availability: make(chan bool, 16)}
	web := &sessionAudioServer{pcm: make(chan capture.AudioFrame, 4), availability: make(chan bool, 16)}
	b.SetRDP(rdp)
	b.SetWS(web, web)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	b.StartRDPWithCodec("clearcodec")
	first.waitForRead(t)
	checkPCM := func(device *sessionAudioDevice, value byte) {
		t.Helper()
		device.frames <- capture.AudioFrame{PCM: []byte{value, 0, 0, 0}, SampleRate: 48000, Channels: 2}
		select {
		case f := <-rdp.pcm:
			if f.PCM[0] != value || len(f.Encoded) != 0 {
				t.Fatal("wrong RDP audio")
			}
		case <-time.After(time.Second):
			t.Fatal("RDP audio did not arrive")
		}
	}
	checkPCM(audio1, 10)
	b.SwitchMachine(1)
	second.waitForRead(t)
	checkPCM(audio2, 20)
	b.OnClientDisconnected()
	for _, device := range []*sessionAudioDevice{audio1, audio2} {
		device.mu.Lock()
		opens, closes := device.opens, device.closes
		device.mu.Unlock()
		if opens != 1 || closes != 1 {
			t.Fatalf("audio device lifecycle = %d/%d", opens, closes)
		}
	}
	var states []bool
	for len(rdp.availability) > 0 {
		states = append(states, <-rdp.availability)
	}
	if len(states) != 4 || !states[0] || states[1] || !states[2] || states[3] {
		t.Fatalf("RDP audio availability = %v", states)
	}
	if len(web.pcm) != 0 || len(web.availability) != 0 {
		t.Fatal("RDP audio modified Web sink")
	}
}
