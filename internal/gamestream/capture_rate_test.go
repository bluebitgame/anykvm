package gamestream

import (
	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"anykvm/internal/framerate"
	"anykvm/internal/videoenc"
	"context"

	"testing"
	"time"
)

type rateTestEncoder struct{ calls chan struct{} }

func (e *rateTestEncoder) Name() string { return "test" }
func (e *rateTestEncoder) Close()       {}
func (e *rateTestEncoder) Encode([]byte, bool) ([]byte, error) {
	e.calls <- struct{}{}
	return []byte{0, 0, 0, 1, 0x65}, nil
}
func (e *rateTestEncoder) EncodePixels([]byte, int, int, string, bool) ([]byte, error) {
	return e.Encode(nil, false)
}
func TestCaptureCardNeverUpscalesOrDuplicates(t *testing.T) {
	for _, tc := range []struct {
		source       framerate.Rate
		target, want int
	}{{framerate.New(30, 1), 60, 30}, {framerate.New(60, 1), 50, 60}, {framerate.New(60, 1), 30, 30}, {framerate.New(60, 1), 20, 20}, {framerate.New(60000, 1001), 30, 30}, {framerate.New(120000, 1001), 30, 30}, {framerate.New(30000, 1001), 60, 30}} {
		ctx, cancel := context.WithCancel(context.Background())
		x := &streamSession{ctx: ctx, frames: make(chan bridge.Frame, 120), keyframe: make(chan struct{}), options: streamOptions{Width: 1280, Height: 720, FPS: tc.target, Bitrate: 2345}}
		f := bridge.Frame{PreserveSource: true, CaptureRate: tc.source, Width: 1920, Height: 1080, Format: capture.FormatRaw}
		enc := &rateTestEncoder{calls: make(chan struct{}, 120)}
		done := make(chan error, 1)
		go func() {
			_, err := x.encodeVideoUsing(f, &videoPacketizer{}, time.Now(), func(o videoenc.Options) (videoenc.Encoder, error) {
				rate, _ := tc.source.Decimate(tc.target)
				if o.Rate != rate || o.Width != 1920 || o.Height != 1080 || o.FPS != tc.want || o.Bitrate != 2345 {
					t.Errorf("wrong encoder profile: %+v", o)
				}
				return enc, nil
			})
			done <- err
		}()
		// Approximately one second of source frames, including the initial frame.
		for n := 1; n < tc.source.Round(); n++ {
			x.frames <- f
		}
		for n := 0; n < tc.want; n++ {
			select {
			case <-enc.calls:
			case <-time.After(time.Second):
				t.Error("missing encoded frame")
				cancel()
			}
		}
		select {
		case <-enc.calls:
			t.Error("duplicated frame or non-integral rate conversion")
		case <-time.After(80 * time.Millisecond):
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Fatal("encoder did not stop")
		}
	}
}

func TestUnknownCaptureRateKeepsNegotiatedEncoderHint(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	x := &streamSession{ctx: ctx, frames: make(chan bridge.Frame), keyframe: make(chan struct{}),
		options: streamOptions{Width: 1920, Height: 1080, FPS: 60, Bitrate: 16000}}
	first := bridge.Frame{PreserveSource: true, Format: capture.FormatRaw, Width: 500, Height: 1080}
	enc := &rateTestEncoder{calls: make(chan struct{}, 1)}
	done := make(chan error, 1)
	go func() {
		_, err := x.encodeVideoUsing(first, &videoPacketizer{}, time.Now(), func(o videoenc.Options) (videoenc.Encoder, error) {
			if o.FPS != 60 || o.Rate != framerate.New(60, 1) || o.Width != 500 || o.Height != 1080 {
				t.Errorf("unknown variable cadence replaced negotiated hint: %+v", o)
			}
			return enc, nil
		})
		done <- err
	}()
	select {
	case <-enc.calls:
	case <-time.After(time.Second):
		t.Error("missing initial encode")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
