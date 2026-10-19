package capture

import (
	"anykvm/internal/framerate"
	"anykvm/internal/videoenc"
	"testing"
	"time"
)

func TestNativeCaptureRateUsesActualDeviceCadence(t *testing.T) {
	for _, source := range []framerate.Rate{framerate.New(60, 1), framerate.New(144, 1), framerate.New(60000, 1001)} {
		got, err := nativeCaptureRate(source, 0)
		if err != nil || got != source {
			t.Fatalf("source=%v got=%v err=%v", source, got, err)
		}
		limited, err := nativeCaptureRate(source, 30)
		if err != nil || limited != framerate.New(30, 1) {
			t.Fatalf("explicit FPS lost: %v %v", limited, err)
		}
	}
	if _, err := nativeCaptureRate(framerate.Rate{}, 0); err == nil {
		t.Fatal("unknown device rate silently received an arbitrary FPS")
	}
}
func TestNativeFractionalRateAndConfiguredBitrateReachEncoder(t *testing.T) {
	source := framerate.New(60000, 1001)
	o := videoenc.Options{Width: 4, Height: 4, FPS: 60, Rate: source, Bitrate: 25000, Codec: "h264"}
	d := &ffmpegDevice{videoCodec: "h264", reqFPS: 60, requestedRate: source, videoBitrate: 25000, inputFPS: source, encoder: &textureEncoderFixture{}, encodeOptions: o}
	now := time.Now()
	for i := 0; i < 5; i++ {
		n := &textureFixture{handled: true}
		f, err := d.encodeMediaFrame(&Frame{Width: 4, Height: 4, Format: FormatRaw, CapturedAt: now.Add(time.Duration(i) * source.Interval()), nativeBuffer: n})
		if err != nil || f == nil {
			t.Fatalf("physical-rate frame %d was dropped or failed: %v", i, err)
		}
		f.Release()
	}
	if d.encodeOptions != o {
		t.Fatal("encoder changed physical cadence or configured bitrate")
	}
}
