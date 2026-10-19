//go:build cgo

package gamestream

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"anykvm/internal/videoenc"
)

type rawClockEncoder struct{}

func (*rawClockEncoder) Name() string { return "clock fixture" }
func (*rawClockEncoder) Close()       {}
func (*rawClockEncoder) Encode(data []byte, _ bool) ([]byte, error) {
	return append([]byte{0, 0, 0, 1, 0x65}, data[:8]...), nil
}
func (e *rawClockEncoder) EncodePixels(data []byte, _, _ int, _ string, idr bool) ([]byte, error) {
	return e.Encode(data, idr)
}

// Use independent live arrivals and the actual latest-frame queue. Scheduling
// coalesced raw input relative to an old first PTS turns initialization latency
// into a wait on every picture, permanently reducing an otherwise 60 FPS input.
func TestDecodedLiveFramesDoNotReplayStartupDelay(t *testing.T) {
	x, peer, cancel, _ := passiveVideoSession(t)
	base := time.Now().Round(0)
	epoch := base.Add(-time.Millisecond)
	frame := func(captured time.Time) bridge.Frame {
		captured = captured.Round(0) // AirPlay's mapped wall clock has no monotonic component.
		data := make([]byte, 16)
		binary.LittleEndian.PutUint64(data, uint64(captured.UnixNano()))
		return bridge.Frame{Format: capture.FormatRaw, PreserveSource: true,
			Width: 2, Height: 2, CapturedAt: captured, Data: data}
	}
	done := make(chan error, 1)
	go func() {
		_, err := x.encodeVideoUsing(frame(base), &videoPacketizer{}, epoch, func(videoenc.Options) (videoenc.Encoder, error) {
			time.Sleep(120 * time.Millisecond) // Native encoder startup/transition.
			return &rawClockEncoder{}, nil
		})
		done <- err
	}()
	producer := make(chan struct{})
	go func() {
		defer close(producer)
		ticker := time.NewTicker(time.Second / 60)
		defer ticker.Stop()
		for {
			select {
			case <-x.ctx.Done():
				return
			case captured := <-ticker.C:
				x.server.PushFrame(frame(captured))
			}
		}
	}()
	defer func() {
		cancel()
		<-producer
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	count := 0
	for time.Since(base) < 2*time.Second {
		au, pts, _ := receivePassivePicture(t, peer, x.options.PacketSize)
		captured := time.Unix(0, int64(binary.LittleEndian.Uint64(au[5:])))
		if want := captureRTPTimestamp(captured, epoch, time.Time{}); pts != want {
			t.Fatalf("raw scheduling replaced capture time in RTP: got=%d want=%d capture=%v epoch=%v AU=%x", pts, want, captured, epoch, au)
		}
		count++
	}
	if count < 60 {
		t.Fatalf("60 FPS live input stayed throttled after startup: %d pictures in 2s", count)
	}
	t.Logf("delivered %d live pictures in %.2fs", count, time.Since(base).Seconds())
}
