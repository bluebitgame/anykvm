package rdp

import (
	"bytes"
	"image"
	"image/jpeg"
	"io"
	"os"
	"testing"
	"time"

	"anykvm/internal/avmedia"
	"anykvm/internal/bridge"
	"anykvm/internal/capture"
)

func TestAVC420SPSProfiles(t *testing.T) {
	for _, tc := range []struct {
		p    []byte
		want bool
	}{
		{[]byte{0x67, 66, 0, 31, 0x80}, true},
		{[]byte{0x67, 77, 0, 40, 0x80}, true},
		{[]byte{0x67, 100, 0, 40, 0xac}, true},
		{[]byte{0x67, 100, 0, 40, 0xbc}, false},
		{[]byte{0x67, 66, 0, 60, 0x80}, false},
		{[]byte{0x67, 100, 0, 40}, false},
		{[]byte{0x67, 100, 0, 40, 0}, false},
	} {
		if got := avc420SPS(tc.p); got != tc.want {
			t.Fatalf("SPS %x: %v", tc.p, got)
		}
	}
}

func TestH264WaitsForIDR(t *testing.T) {
	v := videoPipeline{waiting: true}
	defer v.close()
	sps := []byte{0, 0, 0, 1, 0x67, 66, 0, 31, 0x80}
	pps := []byte{0, 0, 1, 0x68, 0xce}
	frame := bridge.Frame{Format: capture.FormatH264, Width: 64, Height: 48, Data: join(sps, pps, []byte{0, 0, 1, 0x41, 0x80})}
	out, _, _, err := v.frame(frame, 1000, 30)
	if err != nil || len(out) != 0 {
		t.Fatalf("sent prediction before recovery: %x %v", out, err)
	}
	frame.Data = []byte{0, 0, 1, 0x65, 0x80}
	out, _, _, err = v.frame(frame, 1000, 30)
	if err != nil || !bytes.Contains(out, []byte{0x67, 66}) || !bytes.Contains(out, []byte{0x68, 0xce}) || v.encoder != nil {
		t.Fatal("IDR did not preserve parameter sets or bypass conversion")
	}
	v.waiting = true
	frame.Data = []byte{0, 0, 1, 0x41, 0x80}
	out, _, _, err = v.frame(frame, 1000, 30)
	if err != nil || len(out) != 0 {
		t.Fatal("sent prediction after refresh")
	}
}

func TestH264CaptureScalesToRequestedOutput(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_RDP_MEDIA") == "" {
		t.Skip("set ANYKVM_TEST_RDP_MEDIA=1 and ANYKVM_FFMPEG_LIBDIR to exercise codecs")
	}
	source, err := avmedia.OpenVideoSource("mov", "../testdata/passthrough.mp4", nil, "h264")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	decoder, err := avmedia.NewDecoder("h264")
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	v := videoPipeline{waiting: true, forceIDR: true, outputW: 128, outputH: 80}
	defer v.close()
	count := 0
	for {
		f, err := source.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if f == nil {
			continue
		}
		if count == 2 {
			v.outputW, v.outputH = 160, 96
		}
		data, w, h, err := v.frame(bridge.Frame{Format: capture.FormatH264, Width: f.Width, Height: f.Height, Data: f.Data}, 1000, 30)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) == 0 {
			continue
		}
		decoded, err := decoder.Decode(data)
		if err != nil || decoded == nil || w != v.outputW || h != v.outputH || decoded.Width != w || decoded.Height != h {
			t.Fatalf("requested %dx%d decoded=%v error=%v", v.outputW, v.outputH, decoded, err)
		}
		count++
	}
	if count != 4 || v.encoder == nil {
		t.Fatalf("mismatched H.264 capture bypassed scaling: frames=%d", count)
	}
}

func TestVideoConversionAndDecode(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_RDP_MEDIA") == "" {
		t.Skip("set ANYKVM_TEST_RDP_MEDIA=1 and ANYKVM_FFMPEG_LIBDIR to exercise codecs")
	}
	if err := avmedia.Available(); err != nil {
		t.Fatal(err)
	}
	for _, format := range []capture.VideoFormat{capture.FormatRaw, capture.FormatJPEG, capture.FormatYUV} {
		t.Run(format.String(), func(t *testing.T) {
			v := videoPipeline{forceIDR: true}
			defer v.close()
			f := bridge.Frame{Format: format, Width: 64, Height: 48, Data: make([]byte, 64*48*4)}
			for i := 0; i < len(f.Data); i += 4 {
				f.Data[i], f.Data[i+1], f.Data[i+2], f.Data[i+3] = 80, 130, 180, 255
			}
			if format == capture.FormatJPEG {
				var b bytes.Buffer
				if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 64, 48)), nil); err != nil {
					t.Fatal(err)
				}
				f.Data = b.Bytes()
			}
			if format == capture.FormatYUV {
				f.PixFmt = "nv12"
				f.Data = bytes.Repeat([]byte{128}, 64*48*3/2)
			}
			p, w, h, err := v.frame(f, 1000, 30)
			if err != nil {
				t.Fatal(err)
			}
			decoder, err := avmedia.NewDecoder("h264")
			if err != nil {
				t.Fatal(err)
			}
			defer decoder.Close()
			decoded, err := decoder.Decode(p)
			if err != nil || decoded == nil || decoded.Width != w || decoded.Height != h {
				t.Fatalf("output did not decode: frame=%v error=%v", decoded, err)
			}
		})
	}
	for _, codec := range []string{"h264", "hevc"} {
		t.Run(codec, func(t *testing.T) {
			path := "../testdata/passthrough.mp4"
			format := capture.FormatH264
			if codec == "hevc" {
				path = "../testdata/passthrough_hevc.mp4"
				format = capture.FormatHEVC
			}
			source, err := avmedia.OpenVideoSource("mov", path, nil, codec)
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			v := videoPipeline{waiting: true, forceIDR: true}
			defer v.close()
			decoder, err := avmedia.NewDecoder("h264")
			if err != nil {
				t.Fatal(err)
			}
			defer decoder.Close()
			count := 0
			for {
				f, err := source.Read()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if f == nil {
					continue
				}
				p, w, h, err := v.frame(bridge.Frame{Data: f.Data, Format: format, Width: f.Width, Height: f.Height}, 1000, 30)
				if err != nil {
					t.Fatal(err)
				}
				if len(p) == 0 {
					continue
				}
				decoded, err := decoder.Decode(p)
				if err != nil || decoded == nil || decoded.Width != w || decoded.Height != h {
					t.Fatalf("%s -> AVC420: frame=%v error=%v", codec, decoded, err)
				}
				count++
			}
			if count != 4 {
				t.Fatalf("decoded %d of 4 source frames", count)
			}
			if codec == "h264" && v.encoder != nil {
				t.Fatal("compatible H.264 was unnecessarily re-encoded")
			}
		})
	}
}

func TestH264ConfigurationWaitsBeforeConversion(t *testing.T) {
	v := videoPipeline{waiting: true}
	defer v.close()
	f := bridge.Frame{Format: capture.FormatH264, Width: 64, Height: 48, Data: []byte{0, 0, 1, 0x67, 100, 0, 40, 0xbc, 0, 0, 1, 0x68, 0xce}}
	for n := 0; n < 2; n++ {
		out, _, _, err := v.frame(f, 1000, 30)
		if err != nil || len(out) != 0 || v.decoder != nil {
			t.Fatal("entered decoder before a complete recovery frame")
		}
		f.Data = []byte{0, 0, 1, 0x41, 0x80}
	}
}

func TestNativeH264ResizeWaitsForCaptureIDR(t *testing.T) {
	v := videoPipeline{waiting: true, outputW: 640, outputH: 480}
	defer v.close()
	idr := join([]byte{0, 0, 1, 0x67, 66, 0, 31, 0x80}, []byte{0, 0, 1, 0x68, 0xce}, []byte{0, 0, 1, 0x65, 0x80})
	predicted := []byte{0, 0, 1, 0x41, 0x80}
	check := func(w, h int, data []byte, wantPicture bool) {
		t.Helper()
		out, ow, oh, err := v.frame(bridge.Frame{CaptureScalesOutput: true, Format: capture.FormatH264, Width: w, Height: h, Data: data}, 1000, 30)
		if err != nil || (len(out) > 0) != wantPicture {
			t.Fatalf("capture=%dx%d target=%dx%d picture=%t: bytes=%d error=%v", w, h, v.outputW, v.outputH, wantPicture, len(out), err)
		}
		if wantPicture && (ow != w || oh != h || !bytes.Contains(out, data)) {
			t.Fatal("native access unit or dimensions changed")
		}
		if v.decoder != nil || v.encoder != nil {
			t.Fatal("native resize entered decoding or re-encoding")
		}
	}
	check(640, 480, idr, true)
	v.outputW, v.outputH = 800, 600
	check(640, 480, idr, false)
	check(640, 480, predicted, false)
	check(800, 600, predicted, false)
	check(800, 600, idr, true)
	check(800, 600, predicted, true)
	// Rapid updates leave frames from an intermediate size in flight. Returning
	// to the same SPS must still wait for an IDR after discarding references.
	v.outputW, v.outputH = 1024, 768
	check(800, 600, predicted, false)
	v.outputW, v.outputH = 800, 600
	check(1024, 768, idr, false)
	check(800, 600, predicted, false)
	check(800, 600, idr, true)
	// Display Control permits odd heights; native capture rounds its canvas up.
	v.outputW, v.outputH = 800, 601
	check(800, 600, idr, false)
	check(800, 602, predicted, false)
	check(800, 602, idr, true)
}

func TestH264DisplayControlResizeWithoutConversion(t *testing.T) {
	c := channelTestClient()
	conn := &surfaceTestConn{frames: make(chan struct{}, 4)}
	c.conn, c.codec, c.sent, c.suspendAck = conn, codecAVC420, 0, true
	c.frames, c.desktopWake = make(chan bridge.Frame, 4), make(chan struct{}, 1)
	c.s.active = c
	keyframeRequests := 0
	c.s.cfg.OnKeyframe = func() { keyframeRequests++ }
	c.setOutputSize(640, 480)
	result := make(chan error, 1)
	go func() { result <- c.videoLoop() }()
	defer func() { c.close(); <-result }()
	idr := join([]byte{0, 0, 1, 0x67, 66, 0, 31, 0x80}, []byte{0, 0, 1, 0x68, 0xce}, []byte{0, 0, 1, 0x65, 0x80})
	push := func(w, h int, data []byte) {
		c.frames <- bridge.Frame{CaptureScalesOutput: true, Format: capture.FormatH264, Width: w, Height: h,
			SourceWidth: 1920, SourceHeight: 1080, Data: data}
	}
	wait := func(w, h int) {
		t.Helper()
		select {
		case <-conn.frames:
			if conn.outputW != w || conn.outputH != h {
				t.Fatalf("graphics output=%dx%d, want %dx%d", conn.outputW, conn.outputH, w, h)
			}
		case err := <-result:
			// Preserve completion for deferred cleanup.
			result <- err
			t.Fatalf("video loop terminated during native resize: %v", err)
		case <-time.After(time.Second):
			t.Fatal("native picture not forwarded")
		}
	}
	push(640, 480, idr)
	wait(640, 480)
	for _, size := range [][2]int{{800, 600}, {1024, 769}, {640, 480}} {
		oldW, oldH := conn.outputW, conn.outputH
		if err := c.handleDisplayLayout(testDisplayLayout(size[0], size[1])); err != nil {
			t.Fatal(err)
		}
		push(oldW, oldH, idr)
		push(size[0], (size[1]+1)&^1, []byte{0, 0, 1, 0x41, 0x80})
		push(size[0], (size[1]+1)&^1, idr)
		wait(size[0], (size[1]+1)&^1)
	}
	if keyframeRequests != 0 {
		t.Fatal("old-size frames requested a redundant capture restart")
	}
	// A refresh at the established size still needs a fresh source keyframe.
	c.reset.Store(true)
	push(640, 480, []byte{0, 0, 1, 0x41, 0x80})
	push(640, 480, idr)
	wait(640, 480)
	if keyframeRequests != 1 {
		t.Fatal("refresh did not request a source keyframe")
	}
	if conn.creates != 4 || conn.deletes != 3 {
		t.Fatalf("surface lifecycle: creates=%d deletes=%d", conn.creates, conn.deletes)
	}
}
