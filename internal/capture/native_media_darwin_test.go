//go:build darwin && cgo

package capture

import (
	"anykvm/internal/framerate"
	"bytes"
	"image/jpeg"
	"os"
	"testing"
	"time"
)

// No camera or screen permissions are needed: only encoding uses native APIs.
type nativeMediaSource struct {
	fakeRawSource
	rate        framerate.Rate
	decodeImage bool
}

func (s *nativeMediaSource) SetYUVPassthrough(on bool) {
	s.pixfmt = ""
	if on {
		s.pixfmt = "uyvy422"
	}
}
func (s *nativeMediaSource) CaptureFrameRate() framerate.Rate { return s.rate }
func (s *nativeMediaSource) SetDecodeImage(on bool)           { s.decodeImage = on }

func TestMacNativeMJPEG(t *testing.T) {
	source := &nativeMediaSource{fakeRawSource: fakeRawSource{w: 321, h: 241}, rate: framerate.New(60000, 1001)}
	dev := &smartDevice{native: source, caps: deviceCaps{width: 321, height: 241, fps: 60, ok: true}, jpegQuality: 90}
	if err := dev.OpenWithFormat(321, 241, 60); err != nil {
		t.Fatal(err)
	}
	defer dev.Close()
	if _, ok := dev.activeBackend().(*jpegEncodeDevice); !ok {
		t.Fatal("MJPEG did not select the native wrapper")
	}
	f := captureOne(t, dev, time.Second)
	defer f.Release()
	if f.Format != FormatJPEG || f.Width != 321 || f.Height != 241 {
		t.Fatalf("invalid MJPEG frame: %+v", f)
	}
	img, err := jpeg.Decode(bytes.NewReader(f.Data))
	if err != nil || img.Bounds().Dx() != 321 || img.Bounds().Dy() != 241 {
		t.Fatalf("JPEG decode: %v", err)
	}
	if dev.CaptureFrameRate() != source.rate {
		t.Fatal("JPEG wrapper lost the source's fractional frame rate")
	}
	dev.SetDecodeImage(true)
	f.Release()
	if f = captureOne(t, dev, time.Second); f.Format != FormatRaw {
		t.Fatal("raw consumer received JPEG")
	}
}

type jpegFrameSource struct {
	Device
	frame *Frame
}

func (s *jpegFrameSource) Capture() (*Frame, error) { return s.frame, nil }

func TestJPEGWrapperPassthroughAndMetadata(t *testing.T) {
	frame := &Frame{Data: []byte{0xff, 0xd8}, Format: FormatJPEG, Width: 37, Height: 25, CapturedAt: time.Now(), SourceWidth: 74, SourceHeight: 50}
	source := &jpegFrameSource{frame: frame}
	dev := &jpegEncodeDevice{Device: source}
	got, err := dev.Capture()
	if err != nil || got != frame {
		t.Fatal("device JPEG was re-encoded")
	}
	frame.Format, frame.Data = FormatRaw, make([]byte, 37*25*4)
	got, err = dev.Capture()
	defer got.Release()
	if err != nil || got.Format != FormatJPEG || got.CapturedAt != frame.CapturedAt || got.SourceWidth != 74 || got.SourceHeight != 50 {
		t.Fatalf("metadata lost: %+v, %v", got, err)
	}
	if frame.Format != FormatRaw {
		t.Fatal("wrapper modified source frame")
	}
}

func TestMacMediaWithoutFFmpeg(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_NATIVE_MEDIA") == "" {
		t.Skip("set ANYKVM_TEST_NATIVE_MEDIA=1 with FFmpeg libraries absent")
	}
	if ffmpegAvailable() {
		t.Fatal("FFmpeg libraries are available; this test must verify system APIs alone")
	}
	codecs := []string{"mjpeg", "rawvideo", "uyvy", "h264"}
	if (&screenDevice{}).SupportsGameStreamHEVC() {
		codecs = append(codecs, "hevc")
	}
	for _, codec := range codecs {
		t.Run(codec, func(t *testing.T) {
			source := &nativeMediaSource{fakeRawSource: fakeRawSource{w: 320, h: 240}, rate: framerate.New(60000, 1001)}
			dev := &smartDevice{native: source, caps: deviceCaps{width: 320, height: 240, fps: 60, ok: true}, videoCodec: codec}
			if codec == "h264" || codec == "hevc" {
				dev.streamOptions = &VideoStreamOptions{Codec: codec, Width: 640, Height: 480, FPS: 30, Bitrate: 2000}
			}
			if err := dev.OpenWithFormat(320, 240, 60); err != nil {
				t.Fatal(err)
			}
			defer dev.Close()
			f := captureOne(t, dev, 5*time.Second)
			defer f.Release()
			want := map[string]VideoFormat{"mjpeg": FormatJPEG, "rawvideo": FormatRaw, "uyvy": FormatYUV, "h264": FormatH264, "hevc": FormatHEVC}[codec]
			if f.Format != want || f.Width != 320 || f.Height != 240 || len(f.Data) == 0 {
				t.Fatalf("unexpected native frame: %+v", f)
			}
			if dev.CaptureFrameRate() != source.rate {
				t.Fatal("lost capture rate")
			}
			if codec == "h264" || codec == "hevc" {
				if !source.decodeImage {
					t.Fatal("device JPEGs must decode before hardware transcoding")
				}
				pipe := dev.activeBackend().(*pipeEncoderDevice)
				if pipe.enc.encoder.Name() != "videotoolbox" {
					t.Fatalf("unexpected encoder %s", pipe.enc.encoder.Name())
				}
				if pipe.enc.encodeOptions.Rate != framerate.New(30000, 1001) {
					t.Fatal("incorrect source decimation")
				}
				if err := dev.RequestKeyframe(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestRDPCaptureCardHardwareOutputBounds(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_NATIVE_MEDIA") == "" {
		t.Skip("set ANYKVM_TEST_NATIVE_MEDIA=1 to require hardware encoding")
	}
	for _, target := range [][2]int{{320, 240}, {1280, 960}} {
		// Deliberately fit 16:9 input into a smaller 4:3 client canvas.
		source := &nativeMediaSource{fakeRawSource: fakeRawSource{w: 640, h: 360}, rate: framerate.New(60, 1)}
		dev := &smartDevice{native: source, caps: deviceCaps{width: 640, height: 360, fps: 60, ok: true}, videoCodec: "h264", videoBitrate: 2000}
		dev.SetOutputDownscaleOnly(true)
		dev.SetOutputSize(target[0], target[1])
		if err := dev.OpenWithFormat(640, 360, 60); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = dev.Close() })
		f := captureOne(t, dev, 5*time.Second)
		w, h := BoundedOutputSize(640, 360, target[0], target[1])
		if f.Format != FormatH264 || f.Width != w || f.Height != h || f.SourceWidth != 640 || f.SourceHeight != 360 {
			t.Fatalf("target=%v incorrect dimensions: %+v", target, f)
		}
		if !dev.PreserveCaptureMode() || dev.CaptureFrameRate() != source.rate {
			t.Fatal("output sizing changed capture mode or cadence")
		}
		f.Release()
		if err := dev.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRDPCaptureCardHardwareTargetCeiling(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_NATIVE_MEDIA") == "" {
		t.Skip("set ANYKVM_TEST_NATIVE_MEDIA=1 to require hardware encoding")
	}
	for _, limit := range [][2]int{{320, 240}, {321, 241}} {
		source := &nativeMediaSource{fakeRawSource: fakeRawSource{w: 640, h: 360}, rate: framerate.New(60, 1)}
		dev := &smartDevice{native: source, caps: deviceCaps{width: 640, height: 360, fps: 60, ok: true}, videoCodec: "h264", videoBitrate: 2000}
		dev.SetOutputDownscaleOnly(true)
		dev.SetOutputSize(1280, 960)
		dev.SetOutputSizeLimit(limit[0], limit[1])
		if err := dev.OpenWithFormat(640, 360, 60); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = dev.Close() })
		f := captureOne(t, dev, 5*time.Second)
		if f.Width != 320 || f.Height != 240 || f.SourceWidth != 640 || f.SourceHeight != 360 {
			t.Fatalf("Target=%v failed to cap hardware output: %+v", limit, f)
		}
		f.Release()
		if err := dev.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
