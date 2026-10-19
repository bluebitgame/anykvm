package capture

import (
	"anykvm/internal/avmedia"
	"anykvm/internal/framerate"
	"anykvm/internal/videoenc"
	"bytes"
	"testing"
	"time"
)

func TestCompressedCaptureBypassesEncoderAndRateLimit(t *testing.T) {
	for _, codec := range []string{"h264", "hevc"} {
		d := &ffmpegDevice{streamOptions: &VideoStreamOptions{Codec: codec, Width: 1280, Height: 720, FPS: 30, Bitrate: 1000}, encodePacer: framePacer{next: time.Now().Add(time.Second)}}
		input := &avmedia.Frame{Data: []byte{0, 0, 0, 1, 0x65, 1, 2}, Width: 1920, Height: 1080, Codec: codec}
		f := mediaCaptureFrame(input)
		for i := 0; i < 3; i++ {
			got, err := d.encodeMediaFrame(f)
			if err != nil || got != f || !bytes.Equal(got.Data, input.Data) {
				t.Fatalf("compressed frame changed: %v", err)
			}
		}
		if d.encoder != nil || d.jpegMedia != nil {
			t.Fatal("created encoder for passthrough")
		}
	}
}

func TestCaptureCardRatePolicy(t *testing.T) {
	for _, tc := range []struct{ source, target, want, divisor int }{
		{60, 30, 30, 2}, {60, 20, 20, 3}, {120, 30, 30, 4}, {60, 50, 60, 1}, {30, 60, 30, 1}, {30, 30, 30, 1}, {0, 30, 0, 1},
	} {
		fps, divisor := CaptureCardRate(tc.source, tc.target)
		if fps != tc.want || divisor != tc.divisor {
			t.Fatalf("%d -> %d: got %d / %d", tc.source, tc.target, fps, divisor)
		}
	}
}

func TestCaptureCardFractionalRatePolicy(t *testing.T) {
	for _, tc := range []struct {
		source                framerate.Rate
		target, want, divisor int
	}{
		{framerate.New(60000, 1001), 30, 30, 2}, {framerate.New(60000, 1001), 30, 30, 2},
		{framerate.New(120000, 1001), 30, 30, 4}, {framerate.New(120000, 1001), 60, 60, 2},
		{framerate.New(60000, 1001), 20, 20, 3}, {framerate.New(60000, 1001), 50, 60, 1},
		{framerate.New(30000, 1001), 60, 30, 1}, {framerate.New(30000, 1001), 30, 30, 1},
		{framerate.New(599, 10), 30, 60, 1}, {framerate.New(601, 10), 30, 60, 1},
	} {
		rate, divisor := tc.source.Decimate(tc.target)
		fps := rate.Round()
		if fps != tc.want || divisor != tc.divisor {
			t.Fatalf("%v -> %d: got %d / %d, want %d / %d", tc.source, tc.target, fps, divisor, tc.want, tc.divisor)
		}
	}
}

type capturePolicyEncoder struct{ calls int }

func (e *capturePolicyEncoder) Encode([]byte, bool) ([]byte, error) {
	e.calls++
	return []byte{0, 0, 0, 1, 0x65}, nil
}
func (e *capturePolicyEncoder) EncodePixels([]byte, int, int, string, bool) ([]byte, error) {
	return e.Encode(nil, false)
}
func (e *capturePolicyEncoder) Name() string { return "test" }
func (e *capturePolicyEncoder) Close()       {}
func TestCaptureCardEncoderPreservesDimensionsAndCountsFrames(t *testing.T) {
	for _, tc := range []struct {
		source              framerate.Rate
		target, fps, frames int
	}{
		{framerate.New(60, 1), 30, 30, 30}, {framerate.New(60, 1), 20, 20, 20}, {framerate.New(60, 1), 50, 60, 60}, {framerate.New(30, 1), 60, 30, 60}, {framerate.New(60000, 1001), 30, 30, 30},
	} {
		enc := &capturePolicyEncoder{}
		rate, _ := tc.source.Decimate(tc.target)
		d := &ffmpegDevice{inputFPS: tc.source, streamOptions: &VideoStreamOptions{Width: 1280, Height: 720, FPS: tc.target, Bitrate: 2345}, encoder: enc,
			encodeOptions: videoenc.Options{Width: 1920, Height: 1080, FPS: tc.fps, Rate: rate, Bitrate: 2345, Codec: "h264"}}
		for i := 0; i < 60; i++ {
			f, err := d.encodeMediaFrame(&Frame{Format: FormatRaw, Width: 1920, Height: 1080})
			if err != nil {
				t.Fatal(err)
			}
			if f != nil && (f.Width != 1920 || f.Height != 1080) {
				t.Fatal("scaled source frame")
			}
		}
		if enc.calls != tc.frames {
			t.Fatalf("%v -> %d: encoded %d of 60, want %d", tc.source, tc.target, enc.calls, tc.frames)
		}
	}
}
