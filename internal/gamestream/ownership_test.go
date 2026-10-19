package gamestream

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"anykvm/internal/framebuf"
)

func TestOwnedFramesReplaceCancelAndDrain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	x := &streamSession{ctx: ctx, started: true, frames: make(chan bridge.Frame, 1)}
	s := &Server{session: x}
	var freed atomic.Int32
	frame := func(format capture.VideoFormat) bridge.Frame {
		data := []byte{1, 2, 3}
		return bridge.Frame{Width: 320, Height: 240, Format: format, Data: data, Buffer: framebuf.New(data, func() { freed.Add(1) })}
	}
	f := frame(capture.FormatJPEG)
	s.PushFrame(f)
	queued := <-x.frames
	if &queued.Data[0] != &f.Data[0] {
		t.Fatal("owned frame copied")
	}
	x.frames <- queued
	f.Release()
	f = frame(capture.FormatJPEG)
	s.PushFrame(f)
	f.Release()
	if freed.Load() != 1 {
		t.Fatal("latest frame replacement leaked")
	}
	queued = <-x.frames
	queued.Release()
	f = frame(capture.FormatH264)
	s.PushFrame(f)
	f.Release()
	stop := make(chan struct{})
	returned := make(chan struct{})
	f = frame(capture.FormatH264)
	go func() { s.PushFrameWithStop(f, stop); close(returned) }()
	close(stop)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("cancelled push blocked")
	}
	f.Release()
	cancel()
	x.drainFrames()
	if freed.Load() != 4 {
		t.Fatal("cancel/drain leaked")
	}
}
