package capture

import (
	"anykvm/internal/framerate"
	"anykvm/internal/videoenc"
	"testing"
	"time"
)

// CapturedAt is the USB sample's arrival time, independent of how long the
// preceding frame took to submit. Run without wall-clock sleeps or hardware.
func TestIOSUSBEncodingUsesSourceCadence(t *testing.T) {
	for _, codec := range []string{"h264", "hevc"} {
		for _, sourceFPS := range []int{30, 60} {
			enc := &capturePolicyEncoder{}
			opts := videoenc.Options{Width: 2622, Height: 1206, FPS: 30, Rate: framerate.New(30, 1), Bitrate: 24000, Codec: codec}
			// DAL may report 60 fps even when the phone delivers only 30 fps.
			d := &ffmpegDevice{encoder: enc, encodeOptions: opts, inputFPS: framerate.New(60, 1), variableInputRate: true, streamOptions: &VideoStreamOptions{Width: 1920, Height: 1080, FPS: 30, Bitrate: 24000, Codec: codec}}
			start := time.Now()
			for n := 0; n < sourceFPS*2; n++ {
				jitter := time.Duration(n%3-1) * time.Millisecond
				f, err := d.encodeMediaFrame(&Frame{Format: FormatRaw, Width: 2622, Height: 1206, CapturedAt: start.Add(time.Duration(n)*time.Second/time.Duration(sourceFPS) + jitter)})
				if err != nil {
					t.Fatal(err)
				}
				if f != nil && (f.Width != 2622 || f.Height != 1206 || f.SourceWidth != 2622 || f.SourceHeight != 1206) {
					t.Fatal("iOS USB encoding resized the source")
				}
				f.Release()
			}
			if enc.calls < 59 || enc.calls > 61 {
				t.Fatalf("%s source=%dfps: encoded %d frames over 2s, want about 60", codec, sourceFPS, enc.calls)
			}
		}
	}
}
