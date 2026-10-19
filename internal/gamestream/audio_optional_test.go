package gamestream

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"anykvm/internal/capture"
)

type optionalAudioEncoder struct {
	frames chan []byte
	closed bool
}

func (e *optionalAudioEncoder) Encode(p []byte) ([]byte, error) {
	e.frames <- append([]byte(nil), p...)
	return nil, nil
}
func (e *optionalAudioEncoder) Close() { e.closed = true }

func TestConfiguredAudioStartsEncoderBeforePCM(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	x := &streamSession{server: &Server{cfg: Config{MachineAudio: []bool{true}}}, ctx: ctx, audio: make(chan capture.AudioFrame, 4)}
	x.options.AudioDuration = 5
	enc := &optionalAudioEncoder{frames: make(chan []byte, 8)}
	created := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- x.nativeAudioLoop(func() (nativeOpusEncoder, error) {
			created <- struct{}{}
			return enc, nil
		})
	}()
	select {
	case <-created:
	case <-time.After(time.Second):
		t.Fatal("configured audio did not create encoder before input")
	}
	pcm := bytes.Repeat([]byte{1, 2, 3, 4}, 240)
	x.audio <- capture.AudioFrame{PCM: pcm[:400]}
	select {
	case <-enc.frames:
		t.Fatal("encoded an incomplete PCM packet")
	case <-time.After(40 * time.Millisecond):
	}
	x.audio <- capture.AudioFrame{PCM: pcm[400:]}
	select {
	case got := <-enc.frames:
		if !bytes.Equal(got, pcm) {
			t.Fatal("PCM was padded or lost")
		}
	case <-time.After(time.Second):
		t.Fatal("did not encode PCM")
	}
	select {
	case <-enc.frames:
		t.Fatal("encoded synthetic silence after input stopped")
	case <-time.After(40 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("audio loop did not stop")
	}
	if !enc.closed {
		t.Fatal("encoder not closed")
	}
}

func TestAudioEncoderFailurePreservesSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	x := &streamSession{server: &Server{cfg: Config{MachineAudio: []bool{true}}}, ctx: ctx, audio: make(chan capture.AudioFrame, 1)}
	x.options.AudioDuration = 5
	x.audio <- capture.AudioFrame{PCM: make([]byte, 960)}
	failure := errors.New("encoder unavailable")
	err := x.nativeAudioLoop(func() (nativeOpusEncoder, error) { return nil, failure })
	if !errors.Is(err, failure) {
		t.Fatalf("got %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("audio failure canceled video session")
	}
}

func TestUnconfiguredAudioDoesNotCreateEncoder(t *testing.T) {
	for _, enabled := range [][]bool{nil, {false}} {
		x := &streamSession{server: &Server{cfg: Config{MachineAudio: enabled}}}
		err := x.nativeAudioLoop(func() (nativeOpusEncoder, error) {
			t.Fatal("unconfigured audio created encoder")
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
