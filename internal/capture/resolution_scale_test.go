package capture

import (
	"anykvm/internal/framerate"
	"anykvm/internal/videoenc"
	"math"
	"testing"
)

func TestScreenScaleH264Options(t *testing.T) {
	for _, rdp := range []bool{false, true} {
		w, h := 6, 4
		if rdp {
			w, h = 2, 2
		}
		opts := videoenc.Options{Width: w, Height: h, FPS: 30, Rate: framerate.New(30, 1), Bitrate: 12000, Codec: "h264"}
		d := &ffmpegDevice{device: "screen:0", scalableInput: true, outputScale: .5,
			videoCodec: "h264", reqFPS: 30, encoder: &textureEncoderFixture{}, encodeOptions: opts,
			outputDownscaleOnly: rdp, targetW: 2, targetH: 2,
			streamOptions: &VideoStreamOptions{Width: 1920, Height: 1080, FPS: 30, Bitrate: 12000, Codec: "h264"}}
		frame := &Frame{Width: 12, Height: 8, Format: FormatRaw, nativeBuffer: &textureFixture{handled: true}}
		out, err := d.encodeMediaFrame(frame)
		if err != nil {
			t.Fatal(err)
		}
		if out == nil {
			t.Fatal("no encoded output")
		}
		if out.Width != w || out.Height != h || out.SourceWidth != 12 || out.SourceHeight != 8 {
			t.Fatalf("rdp=%v: output=%+v", rdp, out)
		}
		out.Release()
	}
}

func TestScaledResolution(t *testing.T) {
	for _, tc := range []struct {
		w, h         int
		scale        float64
		wantW, wantH int
	}{
		{2880, 1800, .5, 1440, 900},
		{2880, 1800, .75, 2160, 1350},
		{2732, 2048, .75, 2048, 1536},
		{1800, 2880, .5, 900, 1440},
		{1920, 1080, 1.5, 2880, 1620},
	} {
		w, h, err := ScaledResolution(tc.w, tc.h, tc.scale)
		if err != nil || w != tc.wantW || h != tc.wantH {
			t.Fatalf("%+v: %dx%d %v", tc, w, h, err)
		}
	}
	for _, scale := range []float64{0, -1, .00001, 100, math.Inf(1), math.NaN()} {
		if _, _, err := ScaledResolution(2880, 1800, scale); err == nil {
			t.Fatalf("accepted scale %g", scale)
		}
	}
}

func TestScreenScaleRawFrames(t *testing.T) {
	d := &ffmpegDevice{device: "screen:0", scalableInput: true, videoCodec: "rawvideo"}
	if err := ConfigureMode(d, "0.5@30"); err != nil {
		t.Fatal(err)
	}
	for _, size := range [][2]int{{8, 8}, {12, 8}} {
		w, h := size[0], size[1]
		pixels := make([]byte, w*h*4)
		for i := range pixels {
			pixels[i] = byte(i)
		}
		out, err := d.encodeMediaFrame(&Frame{Width: w, Height: h, Format: FormatRaw, Data: pixels})
		if err != nil {
			t.Fatal(err)
		}
		if out.Width != w/2 || out.Height != h/2 {
			t.Fatalf("scaled output: %dx%d", out.Width, out.Height)
		}
		if out.Data[4] != pixels[8] {
			t.Fatal("raw pixels were not resampled")
		}
		out.Release()
	}
	d.outputDownscaleOnly = true
	d.SetOutputSize(2, 2)
	out, err := d.encodeMediaFrame(&Frame{Width: 8, Height: 8, Format: FormatRaw, Data: make([]byte, 8*8*4)})
	if err != nil {
		t.Fatal(err)
	}
	defer out.Release()
	if out.Width != 2 || out.Height != 2 {
		t.Fatalf("smaller client output ignored: %dx%d", out.Width, out.Height)
	}
	if err := ConfigureMode(d, "@30"); err != nil || d.outputScale != 1 {
		t.Fatalf("scale not cleared: %v", err)
	}
}
