package capture

import (
	"anykvm/internal/avmedia"
	"io"
	"testing"
	"time"
)

func TestFFmpegAudioCloseInterruptsRead(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	dev := &ffmpegAudioDevice{source: &blockingAudioSource{reader}}
	captured := make(chan struct{})
	go func() { dev.Capture(); close(captured) }()
	closed := make(chan struct{})
	go func() { dev.Close(); close(closed) }()
	for _, done := range []<-chan struct{}{closed, captured} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("audio shutdown blocked on Capture")
		}
	}
	if _, err := dev.Capture(); err == nil {
		t.Fatal("Capture succeeded after Close")
	}
}

type blockingAudioSource struct{ reader *io.PipeReader }

func (s *blockingAudioSource) Read() (*avmedia.Frame, error) {
	var b [1]byte
	_, err := s.reader.Read(b[:])
	return nil, err
}
func (s *blockingAudioSource) Close() { s.reader.Close() }
