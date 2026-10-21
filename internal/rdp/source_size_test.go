package rdp

import (
	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"bytes"
	"testing"
	"time"
)

func TestRDPOutputSizeAllowsReductionButNeverEnlarges(t *testing.T) {
	c := &client{}
	for _, tc := range []struct{ requestW, requestH, wantW, wantH int }{
		{2560, 1440, 1920, 1080},
		{1280, 720, 1280, 720},
		{2560, 720, 1920, 1080},
	} {
		c.setOutputSize(tc.requestW, tc.requestH)
		for _, format := range []capture.VideoFormat{capture.FormatRaw, capture.FormatJPEG, capture.FormatYUV, capture.FormatSurface} {
			f := bridge.Frame{Width: 1920, Height: 1080, SourceWidth: 1920, SourceHeight: 1080, Format: format, PreserveSource: true}
			w, h := c.bitmapOutputSize(f, f.Width, f.Height)
			vw, vh := c.videoOutputSize(f)
			if w != tc.wantW || h != tc.wantH || vw != w || vh != h {
				t.Fatalf("format=%v request=%dx%d got bitmap=%dx%d video=%dx%d", format, tc.requestW, tc.requestH, w, h, vw, vh)
			}
		}
	}
	// Source-sized H.264 must no longer wait forever for an oversized layout.
	c.setOutputSize(2560, 1440)
	f := bridge.Frame{Format: capture.FormatH264, Width: 1920, Height: 1080, SourceWidth: 1920, SourceHeight: 1080, CaptureScalesOutput: true}
	w, h := c.videoOutputSize(f)
	v := videoPipeline{outputW: w, outputH: h}
	if v.waitingForCaptureSize(f) {
		t.Fatal("waiting for an impossible upscale")
	}
	// A previously reduced canvas is not enlarged while capture reopens.
	f.Width, f.Height = 1280, 720
	if w, h := c.bitmapOutputSize(f, f.Width, f.Height); w != 1280 || h != 720 {
		t.Fatal("enlarged an old frame")
	}
}

func TestRDPTargetLimitMatchesNativeCaptureAndBitmapOutput(t *testing.T) {
	c := &client{}
	f := bridge.Frame{Format: capture.FormatH264, Width: 1440, Height: 900,
		SourceWidth: 2880, SourceHeight: 1800, CaptureScalesOutput: true, OutputMaxW: 1440, OutputMaxH: 900}
	for _, tc := range []struct{ rw, rh, w, h int }{{2560, 1440, 1440, 900}, {1280, 720, 1280, 720}, {3840, 2160, 1440, 900}} {
		c.setOutputSize(tc.rw, tc.rh)
		w, h := c.videoOutputSize(f)
		if w != tc.w || h != tc.h {
			t.Fatalf("%+v: video=%dx%d", tc, w, h)
		}
		v := videoPipeline{outputW: w, outputH: h}
		current := f
		current.Width, current.Height = tc.w, tc.h
		if v.waitingForCaptureSize(current) {
			t.Fatal("waiting for a client canvas larger than Target")
		}
		for _, format := range []capture.VideoFormat{capture.FormatRaw, capture.FormatYUV, capture.FormatJPEG, capture.FormatSurface} {
			pixels := f
			pixels.Format, pixels.Width, pixels.Height = format, 2880, 1800
			if bw, bh := c.bitmapOutputSize(pixels, pixels.Width, pixels.Height); bw != w || bh != h {
				t.Fatalf("format=%v bitmap=%dx%d video=%dx%d", format, bw, bh, w, h)
			}
		}
	}
	c.setOutputSize(2560, 1440)
	f.OutputMaxW, f.OutputMaxH = 1441, 901
	if w, h := c.videoOutputSize(f); w != 1440 || h != 900 {
		t.Fatalf("AVC padding exceeded odd limit: %dx%d", w, h)
	}
	f.PreserveSource, f.CaptureScalesOutput = true, false
	f.Width, f.Height = 2560, 1440
	if w, h := c.videoOutputSize(f); w > 1441 || h > 901 {
		t.Fatal("passthrough bypassed explicit Target ceiling")
	}
	f.OutputMaxW = 1
	v := videoPipeline{outputW: 1, outputH: 900}
	if data, _, _, err := v.frame(f, 1000, 60); err == nil || len(data) != 0 {
		t.Fatal("tiny ceiling silently rounded up")
	}
}

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
