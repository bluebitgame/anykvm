package rdp

import (
	"bytes"
	"image"
	"testing"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
)

func TestBitmapScalingAndPointerUseSameContent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		w, h    int
		content image.Rectangle
	}{
		{"pillarbox", 800, 400, image.Rect(200, 0, 600, 400)},
		{"letterbox", 400, 800, image.Rect(0, 200, 400, 600)},
		{"odd size", 401, 802, image.Rect(0, 200, 401, 601)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := bridge.Frame{Width: 100, Height: 100, ContentRect: image.Rect(0, 0, 100, 100)}
			data := bytes.Repeat([]byte{40, 100, 180, 255}, 100*100)
			pixels, output := scaleBitmap(data, f, tc.w, tc.h)
			if output.ContentRect != tc.content || output.Width != tc.w || output.Height != tc.h {
				t.Fatalf("scaled geometry=%+v", output)
			}
			for y := 0; y < tc.h; y++ {
				for x := 0; x < tc.w; x++ {
					want := []byte{0, 0, 0, 255}
					if image.Pt(x, y).In(tc.content) {
						want = []byte{40, 100, 180, 255}
					}
					if !bytes.Equal(pixels[(y*tc.w+x)*4:(y*tc.w+x+1)*4], want) {
						t.Fatalf("unexpected content or border at %d,%d", x, y)
					}
				}
			}
			hid := &mouseRecorder{}
			var input inputState
			input.activate(hid)
			input.frameGeometry(output, tc.w, tc.h)
			for _, p := range []image.Point{tc.content.Min, tc.content.Max.Sub(image.Pt(1, 1))} {
				if err := input.mouse(0x800, uint16(p.X), uint16(p.Y), false); err != nil {
					t.Fatal(err)
				}
			}
			if hid.positions[0] != [2]int{0, 0} || hid.positions[1] != [2]int{32767, 32767} {
				t.Fatalf("mouse=%v", hid.positions)
			}
		})
	}
}

func TestBitmapScalingInterpolatesAndPreservesMetadata(t *testing.T) {
	f := bridge.Frame{Width: 2, Height: 2, ContentRect: image.Rect(0, 0, 1, 2)}
	data := []byte{0, 0, 0, 255, 200, 200, 200, 255, 0, 0, 0, 255, 200, 200, 200, 255}
	out, scaled := scaleBitmap(data, f, 4, 4)
	if scaled.ContentRect != image.Rect(0, 0, 2, 4) {
		t.Fatalf("metadata=%v", scaled.ContentRect)
	}
	if out[4] != 50 || out[8] != 150 {
		t.Fatalf("interpolation=%v", out[:16])
	}
}

func TestOutputResizeRequeuesIdleSource(t *testing.T) {
	c := &client{frames: make(chan bridge.Frame, 1)}
	c.setOutputSize(1920, 1080)
	f := bridge.Frame{Format: capture.FormatRaw, Width: 640, Height: 480, Data: []byte{1}}
	c.lastSource = &f
	c.setOutputSize(1280, 720)
	if w, h := c.requestedOutputSize(640, 480); w != 1280 || h != 720 || !c.reset.Load() {
		t.Fatal("requested output not updated")
	}
	select {
	case frame := <-c.frames:
		if frame.Width != 640 || frame.Height != 480 {
			t.Fatal("source capture size changed")
		}
	default:
		t.Fatal("idle picture not queued after output resize")
	}
	c.setOutputSize(1280, 720)
	if len(c.frames) != 0 {
		t.Fatal("same output repeated a picture")
	}
}
