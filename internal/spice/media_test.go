package spice

import (
	"anykvm/internal/capture"
	"bytes"
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"
)

func pcm(samples ...int16) []byte {
	var out []byte
	for _, sample := range samples {
		out = binary.LittleEndian.AppendUint16(out, uint16(sample))
	}
	return out
}
func packet(samples ...int16) audioPacket {
	return audioPacket{pcm: pcm(samples...), rate: 48000, channels: 2, epoch: 1, volume: [2]uint16{65535, 65535}}
}
func (s *fakeSession) setAudioEnabled(v bool) { s.audioOn = v; s.audioQueue = nil }
func (s *fakeSession) nextAudio() (*audioPacket, error) {
	if s.audioErr != nil {
		return nil, s.audioErr
	}
	if !s.audioOn || !s.audioActive || len(s.audioQueue) == 0 {
		return nil, nil
	}
	p := s.audioQueue[0]
	s.audioQueue = s.audioQueue[1:]
	return &p, nil
}
func (s *fakeSession) audioAvailable() bool             { return s.audioOn && s.audioActive && !s.closed }
func (s *fakeSession) cursor() (*cursorSnapshot, error) { return s.cursorState, nil }

func TestPlaybackFollowsVideoSessionWithoutOwningIt(t *testing.T) {
	d, s, _ := fixture(t)
	audio := d.AudioDevice()
	if err := audio.Open(); err != nil {
		t.Fatal(err)
	}
	s.audioActive = true
	s.audioQueue = append(s.audioQueue, packet(123, -456, 32767, -32768))
	frame, err := audio.Capture()
	if err != nil || frame == nil {
		t.Fatalf("audio: %v", err)
	}
	if frame.SampleRate != 48000 || frame.Channels != 2 || !bytes.Equal(frame.PCM, pcm(123, -456, 32767, -32768)) || frame.Epoch == 0 {
		t.Fatalf("unexpected PCM: %+v", frame)
	}
	if !audio.(interface{ AudioAvailable() bool }).AudioAvailable() {
		t.Fatal("active playback not exposed")
	}
	s.audioActive = false
	if audio.(interface{ AudioAvailable() bool }).AudioAvailable() {
		t.Fatal("playback stop not exposed")
	}
	audio.Close()
	if s.closed || s.audioOn {
		t.Fatal("audio close destroyed video or left audio enabled")
	}
	if _, err := audio.Capture(); !errors.Is(err, capture.ErrStreamStopped) {
		t.Fatalf("closed capture: %v", err)
	}
	enqueue(s, nil)
	f, err := d.Capture()
	if err != nil || f == nil {
		t.Fatalf("video after audio close: %v", err)
	}
	f.Release()
}
func TestPlaybackRestartAndErrorDoNotCorruptVideo(t *testing.T) {
	d, s, _ := fixture(t)
	audio := d.AudioDevice()
	audio.Open()
	defer audio.Close()
	s.audioActive = true
	s.audioQueue = []audioPacket{packet(1000, 1000, 2000, 2000)}
	first, err := audio.Capture()
	if err != nil {
		t.Fatal(err)
	}
	s.audioErr = errors.New("unsupported audio format")
	if _, err = audio.Capture(); err == nil {
		t.Fatal("audio error lost")
	}
	enqueue(s, nil)
	f, err := d.Capture()
	if err != nil || f == nil {
		t.Fatalf("audio error stopped GPU video: %v", err)
	}
	f.Release()
	d.Close()
	if cur, err := d.CaptureCursor(); err != nil || cur.Width != 0 {
		t.Fatal("stale cursor after close")
	}
	next := &fakeSession{audioActive: true}
	d.openSession = func(string, string, string) (nativeSession, error) { return next, nil }
	if err = d.Open(); err != nil {
		t.Fatal(err)
	}
	if !next.audioOn {
		t.Fatal("open audio consumer did not follow reopened video session")
	}
	next.audioQueue = []audioPacket{packet(-2000, -2000)}
	second, err := audio.Capture()
	if err != nil || second == nil {
		t.Fatalf("reopened audio: %v", err)
	}
	if first.Epoch == second.Epoch || !bytes.Equal(second.PCM, pcm(-2000, -2000)) {
		t.Fatal("resampling state or epoch leaked across reconnect")
	}
}
func TestPCMResamplingPreservesPacketBoundariesAndTime(t *testing.T) {
	raw := make([]byte, 4410*2)
	for i := 0; i < 4410; i++ {
		binary.LittleEndian.PutUint16(raw[i*2:], uint16(int16(i*11-22000)))
	}
	p := audioPacket{pcm: raw, rate: 44100, channels: 1, epoch: 7, volume: [2]uint16{65535, 65535}}
	var whole, chunked pcmConverter
	expected, err := whole.convert(p)
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	offset := 0
	for _, frames := range []int{1, 17, 31, 107, 1, 1000, 3253} {
		current := p
		current.pcm = raw[offset*2 : (offset+frames)*2]
		current.sample = uint64(offset)
		frame, err := chunked.convert(current)
		if err != nil {
			t.Fatal(err)
		}
		if frame != nil {
			if frame.Timestamp != int64(len(got)/4)*1000000/48000 {
				t.Fatal("timestamp drift across packet boundaries")
			}
			got = append(got, frame.PCM...)
		}
		offset += frames
	}
	if offset != 4410 || !bytes.Equal(got, expected.PCM) {
		t.Fatal("resampling changed with packet boundaries")
	}
	if frames := len(got) / 4; frames < 4798 || frames > 4800 {
		t.Fatalf("incorrect resampling ratio: %d", frames)
	}
	for i := 0; i < len(got); i += 4 {
		if !bytes.Equal(got[i:i+2], got[i+2:i+4]) {
			t.Fatal("mono was not duplicated to stereo")
		}
	}
}
func TestPCMVolumeMuteAndDiscontinuity(t *testing.T) {
	var converter pcmConverter
	p := packet(30000, -30000, 30000, -30000)
	p.volume = [2]uint16{65535, 0}
	f, err := converter.convert(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f.PCM, pcm(30000, 0, 30000, 0)) {
		t.Fatal("per-channel volume not applied")
	}
	p.sample = 2
	p.mute = true
	f, err = converter.convert(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f.PCM, make([]byte, len(f.PCM))) {
		t.Fatal("mute leaked cached unmuted samples")
	}
	p = packet(-1200, 1200)
	p.sample = 100 // queue drop: must not interpolate across the gap
	f, err = converter.convert(p)
	if err != nil {
		t.Fatal(err)
	}
	if f.Timestamp != 100*1000000/48000 || !bytes.Equal(f.PCM, pcm(-1200, 1200)) {
		t.Fatal("queue discontinuity did not reset interpolation/time")
	}
	p.sample = 0
	p.epoch++
	f, err = converter.convert(p)
	if err != nil || f.Timestamp != 0 {
		t.Fatal("playback restart did not reset time")
	}
	for _, bad := range []audioPacket{{pcm: pcm(1), rate: 0, channels: 1}, {pcm: []byte{1}, rate: 48000, channels: 1}, {pcm: pcm(1, 2, 3), rate: 48000, channels: 3}, {pcm: make([]byte, 48000), rate: 48000, channels: 2}} {
		if _, err = converter.convert(bad); err == nil {
			t.Fatal("accepted unsupported PCM packet")
		}
	}
}
func TestCursorShapesAreOwnedBGRAAndFollowVisibility(t *testing.T) {
	d, s, _ := fixture(t)
	d.width, d.height = 1920, 1080
	s.cursorState = &cursorSnapshot{rgba: []byte{64, 32, 16, 128}, width: 1, height: 1, hotX: 99, hotY: -4, known: true, visible: true, premultiplied: true, serial: 1}
	first, err := d.CaptureCursor()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Image, []byte{16, 32, 64, 128}) || !first.Premultiplied || first.HotX != 0 || first.HotY != 0 || !first.Changed || first.SourceWidth != 1920 {
		t.Fatalf("bad cursor: %+v", first)
	}
	first.Image[0] = 255
	next, err := d.CaptureCursor()
	if err != nil || next.Image[0] != 16 || next.Changed {
		t.Fatal("cursor image is borrowed or stable shape reported changed")
	}
	s.cursorState.visible = false
	s.cursorState.serial++
	hidden, err := d.CaptureCursor()
	if err != nil || hidden.Width != 0 || len(hidden.Image) != 0 || hidden.SystemDefault || !hidden.Changed {
		t.Fatal("hide did not remove previous cursor")
	}
	s.cursorState.visible = true
	s.cursorState.serial++
	restored, err := d.CaptureCursor()
	if err != nil || restored.Width != 1 || !restored.Changed {
		t.Fatal("show/move failed to restore shape")
	}
	s.cursorState = &cursorSnapshot{known: true, visible: true, systemDefault: true, serial: 4}
	def, err := d.CaptureCursor()
	if err != nil || !def.SystemDefault {
		t.Fatal("default cursor lost")
	}
	d.Close()
	closed, err := d.CaptureCursor()
	if err != nil || closed.Width != 0 || closed.SystemDefault || !closed.Changed {
		t.Fatal("source close left stale cursor")
	}
}
func TestCursorValidationAndHiddenState(t *testing.T) {
	for _, bad := range []cursorSnapshot{{known: true, visible: true, width: 1, height: 1}, {known: true, visible: true, width: 257, height: 1, rgba: make([]byte, 257*4)}} {
		if _, err := cursorState(bad); err == nil {
			t.Fatal("accepted invalid cursor")
		}
	}
	for _, state := range []cursorSnapshot{{}, {known: true}, {known: true, visible: false, rgba: []byte{1, 2, 3, 4}, width: 1, height: 1}} {
		got, err := cursorState(state)
		if err != nil || len(got.Image) != 0 || got.SystemDefault {
			t.Fatal("hidden/unknown cursor should not expose stale image")
		}
	}
}

func TestAudioReadsWhileGPUEncoderBusy(t *testing.T) {
	d, s, e := fixture(t)
	audio := d.AudioDevice()
	audio.Open()
	defer audio.Close()
	s.audioActive = true
	s.audioQueue = []audioPacket{packet(100, -100)}
	e.inEncode = make(chan struct{})
	e.release = make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(e.release) }) }
	defer release()
	enqueue(s, nil)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		f, _ := d.Capture()
		if f != nil {
			f.Release()
		}
	}()
	<-e.inEncode
	received := make(chan error, 1)
	go func() {
		f, err := audio.Capture()
		if err == nil && f == nil {
			err = errors.New("missing audio frame")
		}
		received <- err
	}()
	select {
	case err := <-received:
		if err != nil {
			release()
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		release()
		t.Fatal("audio waits for the GPU encoding lock")
	}
	release()
	<-finished
	if d.Stats().AudioFrames != 1 {
		t.Fatal("audio counter not updated independently")
	}
}
