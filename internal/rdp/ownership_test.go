package rdp

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"anykvm/internal/framebuf"
)

func TestOwnedCaptureQueueCacheAndShutdown(t *testing.T) {
	conn, peer := net.Pipe()
	defer peer.Close()
	c := &client{conn: conn, done: make(chan struct{}), frames: make(chan bridge.Frame, 2)}
	s := &Server{active: c}
	c.s = s
	defer c.close()
	var freed atomic.Int32
	frame := func(format capture.VideoFormat) bridge.Frame {
		data := []byte{1, 2, 3}
		return bridge.Frame{Format: format, Data: data, Buffer: framebuf.New(data, func() { freed.Add(1) })}
	}
	f := frame(capture.FormatJPEG)
	s.PushFrame(f)
	if &c.lastSource.Data[0] != &f.Data[0] {
		t.Fatal("cache copied owned frame")
	}
	f.Release()
	f = frame(capture.FormatJPEG)
	s.PushFrame(f)
	f.Release()
	if freed.Load() != 1 {
		t.Fatal("replaced queue/cache leaked")
	}
	queued := <-c.frames
	c.outputSize.Store(uint64(320)<<32 | 240)
	c.setOutputSize(640, 480)
	if len(c.frames) != 1 {
		t.Fatal("cached capture was not requeued")
	}
	queued.Release()
	if freed.Load() != 1 {
		t.Fatal("requeue lost cache ownership")
	}
	f = frame(capture.FormatH264)
	s.PushFrame(f)
	f.Release()
	// Fill the ordered queue, then cancel a producer and close it concurrently.
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
	c.close()
	if freed.Load() != 4 {
		t.Fatalf("shutdown retained %d frames", 4-freed.Load())
	}
}

func TestBitmapReplacementReleasesHeldFrame(t *testing.T) {
	c := &client{done: make(chan struct{}), frames: make(chan bridge.Frame, 2)}
	var freed atomic.Int32
	data := []byte{1}
	first := bridge.Frame{Width: 64, Height: 64, Data: data, Buffer: framebuf.New(data, func() { freed.Add(1) })}
	c.frames <- bridge.Frame{Width: 64, Height: 64, Data: []byte{2}}
	f, dropped := c.latestCapture(first)
	if dropped != 1 || f.Data[0] != 2 || freed.Load() != 1 {
		t.Fatal("presentation replacement leaked")
	}
	f.Release()
}
