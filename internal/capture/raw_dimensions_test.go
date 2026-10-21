package capture

import (
	"testing"
)

func TestBoundedOutputSizeAllowsDownscaleOnly(t *testing.T) {
	for _, tc := range []struct{ w, h, tw, th, wantW, wantH int }{
		{1920, 1080, 1280, 720, 1280, 720},
		{1920, 1080, 2560, 1440, 1920, 1080},
		{1920, 1080, 2560, 720, 1920, 1080},
		{1920, 1080, 1280, 1440, 1920, 1080},
		{1206, 2622, 0, 0, 1206, 2622},
	} {
		w, h := BoundedOutputSize(tc.w, tc.h, tc.tw, tc.th)
		if w != tc.wantW || h != tc.wantH {
			t.Fatalf("%+v: got %dx%d", tc, w, h)
		}
	}
}

func TestLimitedOutputSizeRespectsTargetAndActualSource(t *testing.T) {
	for _, tc := range []struct{ sw, sh, w, h, mw, mh, wantW, wantH int }{
		{2880, 1800, 2560, 1440, 1440, 900, 1440, 900},
		{2880, 1800, 1280, 720, 1440, 900, 1280, 720},
		{1280, 1080, 2560, 1440, 1440, 900, 1280, 900},
		{2880, 1800, 0, 0, 1440, 900, 1440, 900},
		{2880, 1800, 2560, 1440, 0, 0, 2560, 1440},
	} {
		w, h := LimitedOutputSize(tc.sw, tc.sh, tc.w, tc.h, tc.mw, tc.mh)
		if w != tc.wantW || h != tc.wantH {
			t.Fatalf("%+v: got %dx%d", tc, w, h)
		}
	}
}

func TestRDPDownscalePolicyDoesNotEnlargeRawCapture(t *testing.T) {
	for _, target := range [][2]int{{128, 72}, {640, 480}} {
		d := &ffmpegDevice{videoCodec: "rawvideo", scalableInput: true, targetW: target[0], targetH: target[1], outputDownscaleOnly: true}
		source := &Frame{Format: FormatRaw, Width: 320, Height: 240, Data: make([]byte, 320*240*4)}
		f, err := d.encodeMediaFrame(source)
		if err != nil {
			t.Fatal(err)
		}
		if target[0] > source.Width {
			if f != source {
				t.Fatal("oversized output copied or enlarged the source")
			}
		} else if f.Width != 96 || f.Height != 72 {
			t.Fatalf("downscale did not preserve aspect: %dx%d", f.Width, f.Height)
		}
	}
}

func TestRawCaptureUsesScaledOutputDimensions(t *testing.T) {
	d := &ffmpegDevice{videoCodec: "rawvideo", scalableInput: true, targetW: 1280, targetH: 720}
	source := &Frame{Format: FormatRaw, Width: 1920, Height: 1200, Data: make([]byte, 1920*1200*4)}
	f, err := d.encodeMediaFrame(source)
	if err != nil {
		t.Fatal(err)
	}
	if f == nil || f.Width != 1152 || f.Height != 720 || len(f.Data) != 1152*720*4 {
		t.Fatal("incorrect scaled frame dimensions")
	}
}

func TestRawCaptureCardPreservesSourceDimensions(t *testing.T) {
	d := &ffmpegDevice{videoCodec: "rawvideo", targetW: 1280, targetH: 720}
	source := &Frame{Format: FormatRaw, Width: 1206, Height: 2622, Data: make([]byte, 1206*2622*4)}
	f, err := d.encodeMediaFrame(source)
	if err != nil || f != source {
		t.Fatalf("capture-card pixels were resized or copied: %v", err)
	}
}

func TestOutputDimensionsIgnoreInputAndPartialLines(t *testing.T) {
	input := "Input #0, avfoundation, from '1:none':\n  Stream #0:0: Video: rawvideo, uyvy422, 2560x1600, 30 fps\n"
	output := "Output #0, rawvideo, to 'pipe:1':\n  Stream #0:0: Video: rawvideo, bgra, 1152x720, 30 fps"
	for _, log := range []string{input, input + output, input + "Output #0, rawvideo, to 'pipe:1':\n"} {
		if w, h := parseOutputSizeFromLog(log); w != 0 || h != 0 {
			t.Fatalf("accepted premature dimensions %dx%d", w, h)
		}
	}
	if w, h := parseOutputSizeFromLog(input + output + "\n"); w != 1152 || h != 720 {
		t.Fatalf("output size %dx%d", w, h)
	}
}
