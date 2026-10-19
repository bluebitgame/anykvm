//go:build darwin

package capture

import (
	"bytes"
	"context"
	"image/jpeg"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestScreenRDPRangeFallbackPersistsAcrossRestarts(t *testing.T) {
	d := newScreenDevice("screen:0").(*screenDevice)
	d.SetRDPAVC420(true)
	d.fullRangeDisabled = true
	d.SetRDPAVC420(true) // Resize/recovery stays in the same protocol.
	if !d.fullRangeDisabled {
		t.Fatal("restart retried a rejected full-range path")
	}
	d.SetRDPAVC420(false) // GameStream or bitmap capture uses its original mode.
	if d.fullRangeDisabled || d.rdpAVC420 {
		t.Fatal("protocol switch retained RDP range state")
	}
	d.SetRDPAVC420(true)
	if d.fullRangeDisabled || !d.rdpAVC420 {
		t.Fatal("new RDP session did not enable direct full range")
	}
}

// Explicit opt-in: verify the production SCK -> hardware encoder path in
// memory. No display mode changes or screen images are saved.
func TestNativeRDPScreenFullRange(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_SCREEN") != "1" {
		t.Skip("set ANYKVM_TEST_SCREEN=1 with Screen Recording permission")
	}
	d := newScreenDevice("screen:0").(*screenDevice)
	d.SetVideoCodec("h264")
	d.SetDecodeImage(false)
	d.SetRDPAVC420(true)
	for attempt := 0; attempt < 2; attempt++ {
		if err := d.OpenWithFormat(1280, 720, 30); err != nil {
			t.Fatal(err)
		}
		func() {
			defer d.Close()
			deadline := time.Now().Add(8 * time.Second)
			for time.Now().Before(deadline) {
				frame, err := d.Capture()
				if err != nil {
					t.Fatal(err)
				}
				if frame != nil {
					defer frame.Release()
					if d.fullRangeDisabled || frame.Format != FormatH264 {
						t.Fatal("full-range capture fell back instead of passing native verification")
					}
					_, detail := d.CaptureIdleStatus()
					if !strings.Contains(detail, "rdp_full_range=1 range_verified=1") {
						t.Fatalf("native output not verified: %s", detail)
					}
					t.Log(detail)
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
			t.Fatal("no verified full-range H.264 frame")
		}()
	}
}

// Explicit opt-in: captures the real display in memory; requires Screen
// Recording permission. FFmpeg is used only as an independent test decoder.
func TestNativeScreenCapture(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_SCREEN") != "1" {
		t.Skip("set ANYKVM_TEST_SCREEN=1 with Screen Recording permission")
	}
	ff := os.Getenv("ANYKVM_TEST_FFMPEG")
	for _, codec := range []string{"rawvideo", "nv12", "mjpeg", "h264", "hevc"} {
		t.Run(codec, func(t *testing.T) {
			d := newScreenDevice("screen:0").(*screenDevice)
			d.SetVideoCodec(codec)
			d.SetDecodeImage(false)
			d.SetHideCursor(true)
			// Reuse a device across close/reopen, as client switches do.
			for attempt := 0; attempt < 2; attempt++ {
				if err := d.OpenWithFormat(320, 240, 30); err != nil {
					t.Fatal(err)
				}
				func() {
					defer d.Close()
					deadline := time.Now().Add(5 * time.Second)
					var f *Frame
					for time.Now().Before(deadline) {
						var err error
						f, err = d.Capture()
						if err != nil {
							t.Fatal(err)
						}
						if f != nil {
							break
						}
						time.Sleep(2 * time.Millisecond)
					}
					if f == nil {
						t.Fatal("no complete frame in five seconds")
					}
					defer f.Release()
					if f.Width != 320 || f.Height != 240 || f.SourceWidth <= 0 || f.SourceHeight <= 0 {
						t.Fatalf("invalid frame geometry: %dx%d source %dx%d", f.Width, f.Height, f.SourceWidth, f.SourceHeight)
					}
					switch codec {
					case "rawvideo":
						if f.Format != FormatRaw || len(f.Data) != 320*240*4 {
							t.Fatal("invalid BGRA frame")
						}
					case "nv12":
						if f.Format != FormatYUV || f.PixFmt != "nv12" || len(f.Data) != 320*240*3/2 {
							t.Fatal("invalid native NV12 frame")
						}
					case "mjpeg":
						if f.Format != FormatJPEG {
							t.Fatal("expected JPEG")
						}
						img, err := jpeg.Decode(bytes.NewReader(f.Data))
						if err != nil || img.Bounds().Dx() != 320 || img.Bounds().Dy() != 240 {
							t.Fatalf("invalid JPEG: %v", err)
						}
					default:
						want := FormatH264
						if codec == "hevc" {
							want = FormatHEVC
						}
						if f.Format != want || !bytes.HasPrefix(f.Data, []byte{0, 0, 0, 1}) {
							t.Fatal("invalid Annex-B frame")
						}
						if ff == "" {
							t.Log("set ANYKVM_TEST_FFMPEG for independent decode validation")
							return
						}
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()
						cmd := exec.CommandContext(ctx, ff, "-hide_banner", "-loglevel", "error", "-xerror", "-f", codec, "-i", "pipe:0", "-frames:v", "1", "-f", "rawvideo", "-pix_fmt", "rgb24", "pipe:1")
						cmd.Stdin = bytes.NewReader(f.Data)
						var stderr bytes.Buffer
						cmd.Stderr = &stderr
						raw, err := cmd.Output()
						if err != nil || len(raw) != 320*240*3 {
							t.Fatalf("decode: %v, bytes=%d: %s", err, len(raw), &stderr)
						}
					}
				}()
				if f, err := d.Capture(); err != nil || f != nil {
					t.Fatal("closed device returned a frame/error")
				}
			}
		})
	}
}
