package capture

import (
	"testing"
)

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
