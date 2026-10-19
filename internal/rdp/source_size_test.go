package rdp

import (
	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"bytes"
	"testing"
	"time"
)

func TestRDPFixedSourceSwitchKeepsNativeH264WithoutConversion(t *testing.T) {
	c := channelTestClient()
	conn := &surfaceTestConn{frames: make(chan struct{}, 4)}
	c.conn, c.codec, c.sent, c.suspendAck = conn, codecAVC420, 0, true
	c.frames = make(chan bridge.Frame, 4)
	c.setOutputSize(2560, 1440)
	result := make(chan error, 1)
	go func() { result <- c.videoLoop() }()
	defer func() {
		c.close()
		if err := <-result; err != nil {
			t.Errorf("video loop failed: %v", err)
		}
	}()
	idr := join([]byte{0, 0, 1, 0x67, 66, 0, 31, 0x80}, []byte{0, 0, 1, 0x68, 0xce}, []byte{0, 0, 1, 0x65, 0x80})
	push := func(w, h int, fixed bool) {
		t.Helper()
		c.frames <- bridge.Frame{Format: capture.FormatH264, Width: w, Height: h, PreserveSource: fixed, Data: idr}
		select {
		case <-conn.frames:
			if conn.outputW != w || conn.outputH != h {
				t.Fatalf("output=%dx%d, source=%dx%d", conn.outputW, conn.outputH, w, h)
			}
		case err := <-result:
			result <- err
			t.Fatalf("target switch attempted conversion or terminated session: %v", err)
		case <-time.After(time.Second):
			t.Fatal("target switch produced no frame")
		}
		c.input.mu.Lock()
		iw, ih, sw, sh := c.input.w, c.input.h, c.input.sourceW, c.input.sourceH
		c.input.mu.Unlock()
		if iw != w || ih != h || sw != w || sh != h {
			t.Fatalf("pointer geometry %dx%d / %dx%d does not match source %dx%d", iw, ih, sw, sh, w, h)
		}
	}
	push(2560, 1440, false)
	push(1920, 1080, true)
	push(1280, 1024, true)
	// A layout request remains available to the next local-screen target, but
	// must not force a fixed capture card through an unavailable decoder.
	c.setOutputSize(1600, 900)
	push(1280, 1024, true)
	if w, h := c.requestedOutputSize(0, 0); w != 1600 || h != 900 {
		t.Fatal("fixed source replaced the client's requested layout")
	}
	push(1600, 900, false)
	if conn.creates != 4 || conn.deletes != 3 {
		t.Fatalf("invalid surface lifecycle: creates=%d deletes=%d", conn.creates, conn.deletes)
	}
	// The source access unit remains unchanged through the direct pipeline.
	v := videoPipeline{waiting: true, outputW: 1920, outputH: 1080}
	defer v.close()
	out, w, h, err := v.frame(bridge.Frame{Format: capture.FormatH264, Width: 1920, Height: 1080, Data: idr}, 1000, 60)
	if err != nil || w != 1920 || h != 1080 || !bytes.Contains(out, idr) || v.decoder != nil || v.encoder != nil {
		t.Fatal("native H.264 was decoded or re-encoded", err)
	}
}
