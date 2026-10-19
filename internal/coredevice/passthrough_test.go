package coredevice

import (
	"bytes"
	"net"
	"os"
	"testing"
	"time"

	"anykvm/internal/capture"
)

func TestNativeCodecSelection(t *testing.T) {
	c, err := NewClient("coredevice:test-phone", Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, codec := range []string{"", "mjpeg", "rawvideo", "hevc"} {
		if !c.SupportsVideoCodec(codec) {
			t.Fatalf("supported codec %q was not advertised", codec)
		}
	}
	for _, codec := range []string{"h264", "av1"} {
		if c.SupportsVideoCodec(codec) {
			t.Fatalf("unsupported codec %q was advertised", codec)
		}
	}
	c.SetVideoStreamOptions(&capture.VideoStreamOptions{Codec: "hevc", Width: 1280, Height: 720, FPS: 30, Bitrate: 5000})
	if !c.SupportsGameStreamHEVC() || c.videoCodec.Load() != 2 {
		t.Fatal("GameStream HEVC selected a decoded capture path")
	}
	// Web selects its output before the bridge clears the GameStream profile.
	for _, codec := range []string{"", "rawvideo", "hevc"} {
		c.SetVideoCodec(codec)
		mode := c.videoCodec.Load()
		c.SetVideoStreamOptions(nil)
		if c.videoCodec.Load() != mode {
			t.Fatal("clearing GameStream settings overrode Web codec selection")
		}
	}
}

func TestHEVCPassthroughWithoutDecoder(t *testing.T) {
	fixture, err := os.ReadFile("../testdata/passthrough.hevc")
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Fatal(err)
	}
	sender, err := net.DialUDP("udp6", nil, receiver.LocalAddr().(*net.UDPAddr))
	if err != nil {
		receiver.Close()
		t.Fatal(err)
	}
	defer sender.Close()
	client, err := NewClient("coredevice", Options{})
	if err != nil {
		t.Fatal(err)
	}
	s := &session{client: client, endpoint: &endpoint{address: "::1"}, udp: receiver,
		config:      streamConfig{remoteSSRC: 0x11223344, sourcePort: sender.LocalAddr().(*net.UDPAddr).Port},
		passthrough: true, frames: make(chan *capture.Frame, 1), ready: make(chan struct{}),
		done: make(chan struct{}), workerDone: make(chan struct{}),
	}
	go s.mediaLoop(nil)
	defer func() { s.stop(nil); <-s.workerDone }()
	seq := uint16(1)
	for _, nal := range annexBNALs(fixture) {
		if len(nal) < 2 {
			continue
		}
		vcl := nal[0]>>1&63 < 32
		if _, err := sender.Write(rtpPacket(seq, 1, vcl, nal)); err != nil {
			t.Fatal(err)
		}
		seq++
		if vcl {
			break
		}
	}
	select {
	case frame := <-s.frames:
		defer frame.Release()
		if frame.Format != capture.FormatHEVC || frame.Width != 64 || frame.Height != 48 {
			t.Fatalf("frame = %+v", frame)
		}
		units := annexBNALs(frame.Data)
		if len(units) < 4 || units[0][0]>>1&63 != 32 || units[1][0]>>1&63 != 33 || units[2][0]>>1&63 != 34 {
			t.Fatal("missing decoder parameter sets")
		}
		if bytes.Contains(frame.Data, displayTrailer) {
			t.Fatal("DisplayService trailer leaked to browser")
		}
		if client.width.Load() != 64 || client.height.Load() != 48 {
			t.Fatal("input dimensions did not follow stream")
		}
	case <-s.done:
		t.Fatal(s.failure())
	case <-time.After(2 * time.Second):
		t.Fatal("no HEVC access unit delivered without a decoder")
	}
	// A browser may need an IRAP while the RTP assembler still has valid
	// references. Request PLI without restarting or creating a host decoder.
	client.mu.Lock()
	client.session = s
	client.mu.Unlock()
	if err := client.RequestKeyframe(); err != nil {
		t.Fatal(err)
	}
	_ = sender.SetReadDeadline(time.Now().Add(time.Second))
	feedback := make([]byte, 100)
	for {
		n, err := sender.Read(feedback)
		if err != nil {
			t.Fatal("no requested PLI:", err)
		}
		if n == 56 && feedback[45] == 206 {
			if be32(feedback[48:]) != s.config.localSSRC || be32(feedback[52:]) != s.config.remoteSSRC {
				t.Fatal("PLI targeted the wrong sender")
			}
			break
		}
	}
}

func TestPassthroughPreservesPictureOrder(t *testing.T) {
	s := &session{client: &Client{}, passthrough: true, frames: make(chan *capture.Frame, 1), ready: make(chan struct{}), done: make(chan struct{})}
	s.emit(&capture.Frame{Data: []byte{1}})
	finished := make(chan struct{})
	go func() { s.emit(&capture.Frame{Data: []byte{2}}); close(finished) }()
	select {
	case <-finished:
		t.Fatal("dependent picture replaced unread picture")
	case <-time.After(20 * time.Millisecond):
	}
	if f := <-s.frames; f.Data[0] != 1 {
		t.Fatal("first picture lost")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("ordered delivery blocked")
	}
	if f := <-s.frames; f.Data[0] != 2 {
		t.Fatal("second picture lost")
	}
	s.emit(&capture.Frame{Data: []byte{3}})
	finished = make(chan struct{})
	go func() { s.emit(&capture.Frame{Data: []byte{4}}); close(finished) }()
	close(s.done)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not interrupt full queue")
	}
}

func TestHEVCSPSBounds(t *testing.T) {
	fixture, err := os.ReadFile("../testdata/passthrough.hevc")
	if err != nil {
		t.Fatal(err)
	}
	for _, nal := range annexBNALs(fixture) {
		if nal[0]>>1&63 != 33 {
			continue
		}
		info, err := parseSPS(nal)
		if err != nil || info.Width != 64 || info.Height != 48 || info.DepthLuma != 8 {
			t.Fatalf("SPS=%+v: %v", info, err)
		}
		for n := 0; n < 16 && n < len(nal); n++ {
			if _, err := parseSPS(nal[:n]); err == nil {
				t.Fatalf("accepted truncated %d-byte SPS", n)
			}
		}
		return
	}
	t.Fatal("fixture has no SPS")
}
