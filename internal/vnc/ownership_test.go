package vnc

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"anykvm/internal/capture"
	"anykvm/internal/framebuf"
)

func TestOwnedVNCFramesReplaceSwitchAndClose(t *testing.T) {
	conn, peer := net.Pipe()
	defer peer.Close()
	c := &client{conn: conn, done: make(chan struct{}), wake: make(chan struct{}, 1)}
	c.h264QCond = sync.NewCond(&c.stateMu)
	defer c.close()
	s := &Server{cur: c}
	var freed atomic.Int32
	frame := func(format capture.VideoFormat) Frame {
		data := []byte{1, 2, 3}
		return Frame{Format: format, Data: data, Buffer: framebuf.New(data, func() { freed.Add(1) })}
	}
	f := frame(capture.FormatJPEG)
	s.PushFrame(f)
	if &c.latest.data[0] != &f.Data[0] {
		t.Fatal("owned frame copied")
	}
	f.Buffer.Release()
	f = frame(capture.FormatJPEG)
	s.PushFrame(f)
	f.Buffer.Release()
	if freed.Load() != 1 {
		t.Fatal("coalesced frame leaked")
	}
	f = frame(capture.FormatH264)
	s.PushFrame(f)
	f.Buffer.Release()
	if freed.Load() != 2 {
		t.Fatal("codec switch retained JPEG")
	}
	c.close()
	if freed.Load() != 3 {
		t.Fatal("closed queue retained H264")
	}
	f = frame(capture.FormatJPEG)
	s.PushFrame(f)
	f.Buffer.Release()
	if freed.Load() != 4 {
		t.Fatal("closed client accepted ownership")
	}
}
