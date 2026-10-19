//go:build darwin && cgo

package videoenc

import (
	"os"
	"sort"
	"testing"
	"time"
)

// Diagnostic only: synthetic paced 720p60 input, excluding startup and drain.
// This measures encoder/polling latency, not capture-to-display latency.
func TestVideoToolboxLatency(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_VT_LATENCY") == "" {
		t.Skip("set ANYKVM_TEST_VT_LATENCY=1 for sequential latency comparison")
	}
	for _, codec := range []string{"h264", "hevc"} {
		for _, async := range []bool{false, true} {
			e, err := New(Options{Width: 1280, Height: 720, FPS: 60, Bitrate: 12000, Codec: codec})
			if err != nil {
				t.Fatal(err)
			}
			if e.Name() != "videotoolbox" {
				e.Close()
				t.Fatal("expected native encoder")
			}
			a := e.(AsyncEncoder)
			pixels := make([]byte, 1280*720*3/2)
			for i := 0; i < 1280*720; i++ {
				pixels[i] = 80
			}
			for i := 1280 * 720; i < len(pixels); i++ {
				pixels[i] = 128
			}
			const count = 90
			start := time.Now()
			nextSource := start
			pending := false
			sent, received := 0, 0
			times := make(map[uint64]time.Time)
			var values []float64
			for time.Since(start) < 6*time.Second {
				if sent < count && !time.Now().Before(nextSource) {
					nextSource = time.Now().Add(time.Second / 60)
					pending = true
				}
				admit := true
				if async {
					admit, err = CanSubmit(e)
					if err != nil {
						e.Close()
						t.Fatal(err)
					}
				}
				if pending && sent < count && admit {
					id := uint64(sent + 1)
					for y := 0; y < 32; y++ {
						for x := 0; x < 32; x++ {
							pixels[y*1280+x] = byte(32 + sent%160)
						}
					}
					before := time.Now()
					if async {
						accepted, err := a.Submit(pixels, 1280, 720, "nv12", id, sent == 0)
						if err != nil || !accepted {
							e.Close()
							t.Fatalf("paced submit: %v, %v", accepted, err)
						}
						times[id] = before
					} else {
						if _, err := e.Encode(pixels, sent == 0); err != nil {
							e.Close()
							t.Fatal(err)
						}
						if sent >= 15 && sent < count-15 {
							values = append(values, float64(time.Since(before))/float64(time.Millisecond))
						}
						received++
					}
					sent++
					pending = false
				}
				if async {
					for {
						packet, err := a.Receive()
						if err != nil {
							e.Close()
							t.Fatal(err)
						}
						if packet == nil {
							break
						}
						if packet.Dropped {
							e.Close()
							t.Fatal("dropped benchmark picture")
						}
						if packet.ID > 15 && packet.ID <= count-15 {
							values = append(values, float64(time.Since(times[packet.ID]))/float64(time.Millisecond))
						}
						delete(times, packet.ID)
						packet.Release()
						received++
					}
				}
				if sent == count && received >= count-4 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			e.Close()
			if len(values) != 60 {
				t.Fatalf("incomplete latency samples: %d", len(values))
			}
			sort.Float64s(values)
			t.Logf("%s async=%t samples=%d p50=%.2fms p95=%.2fms", codec, async, len(values), values[len(values)/2], values[len(values)*95/100])
		}
	}
}
