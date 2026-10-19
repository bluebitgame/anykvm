//go:build darwin && cgo

package videoenc

import (
	"anykvm/internal/framerate"
	"bytes"
	"os"
	"testing"
	"time"
)

func TestVideoToolboxAsync(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_NATIVE_MEDIA") == "" {
		t.Skip("set ANYKVM_TEST_NATIVE_MEDIA=1 to require hardware encoding")
	}
	for _, mode := range []string{"h264", "h264_regular_rate_control", "h264_high", "h264_high_regular_rate_control", "hevc"} {
		t.Run(mode, func(t *testing.T) {
			codec := mode
			disableLowLatency := mode == "h264_regular_rate_control" || mode == "h264_high_regular_rate_control"
			profile := "constrained_baseline"
			if mode == "h264_high" || mode == "h264_high_regular_rate_control" {
				profile = "high"
			}
			if disableLowLatency || profile == "high" {
				codec = "h264"
			}
			e, err := openNative(Options{Width: 320, Height: 240, FPS: 60, Rate: framerate.New(60, 1), Bitrate: 2000, Codec: codec, DisableLowLatencyRateControl: disableLowLatency, H264Profile: profile}, "videotoolbox", "")
			if err != nil {
				if codec == "hevc" {
					t.Skipf("HEVC hardware unavailable: %v", err)
				}
				t.Fatal(err)
			}
			defer e.Close()
			a, ok := e.(AsyncEncoder)
			if !ok || e.Name() != "videotoolbox" {
				t.Fatalf("did not select native asynchronous encoder: %s", e.Name())
			}
			// Odd, padded BGRA input also exercises native conversion/scaling.
			pixels := make([]byte, 321*241*4)
			for i := 0; i < len(pixels); i += 4 {
				pixels[i], pixels[i+1], pixels[i+2], pixels[i+3] = 32, 96, 192, 255
			}
			limit := 2
			if codec == "hevc" {
				limit = 1
			}
			for id := 1; id <= limit; id++ {
				accepted, err := a.Submit(pixels, 321, 241, "bgra", uint64(id), id == 1)
				if err != nil || !accepted {
					t.Fatalf("initial submit %d: %v, %v", id, accepted, err)
				}
			}
			if accepted, err := a.Submit(pixels, 321, 241, "bgra", 100, false); err != nil || accepted {
				t.Fatalf("admission limit: %v, %v", accepted, err)
			}
			var held *Packet
			var snapshot []byte
			defer func() { held.Release() }()
			next, received := limit+1, 0
			deadline := time.Now().Add(5 * time.Second)
			for received < 24 && time.Now().Before(deadline) {
				packet, err := a.Receive()
				if err != nil {
					t.Fatal(err)
				}
				if packet == nil {
					time.Sleep(time.Millisecond)
					continue
				}
				received++
				if packet.ID != uint64(received) || packet.Dropped || len(packet.Data) < 5 {
					t.Fatalf("unexpected packet: %+v", packet)
				}
				if accepted, err := a.Submit(pixels, 321, 241, "bgra", uint64(next), next == 12); err != nil || !accepted {
					t.Fatalf("continued submit: %v, %v", accepted, err)
				}
				if received == 1 {
					held = packet
					snapshot = append([]byte(nil), packet.Data...)
					if codec == "h264" {
						// DXVA rejects unconstrained Baseline; verify the actual SPS.
						found := false
						for _, nal := range bytes.Split(packet.Data, []byte{0, 0, 1}) {
							if len(nal) >= 4 && nal[0]&31 == 7 {
								found = true
								valid := nal[1] == 66 && nal[2]&0x40 != 0
								if profile == "high" {
									valid = nal[1] == 100
								}
								if !valid {
									t.Fatalf("expected H.264 %s, got profile_idc=%d constraints=0x%02x", profile, nal[1], nal[2])
								}
							}
						}
						if !found {
							t.Fatal("initial H.264 frame has no SPS")
						}
					}
				} else {
					packet.Release()
				}
				next++
			}
			if received != 24 {
				t.Fatalf("asynchronous pipeline stalled after %d frames", received)
			}
			// Close with outstanding submissions, then close again and reject reads.
			e.Close()
			if held.Buffer == nil || !bytes.Equal(held.Data, snapshot) {
				t.Fatal("borrowed packet changed or died with encoder")
			}
			e.Close()
			if _, err := a.Receive(); err == nil {
				t.Fatal("closed encoder accepted receive")
			}
		})
	}
}
