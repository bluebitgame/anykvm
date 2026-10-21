//go:build darwin && cgo

package capture

import (
	"os"
	"testing"
	"time"
)

func TestNativeScreenScale(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_SCREEN") != "1" {
		t.Skip("set ANYKVM_TEST_SCREEN=1 with Screen Recording permission")
	}
	for _, codec := range []string{"rawvideo", "h264"} {
		t.Run(codec, func(t *testing.T) {
			d := newScreenDevice("screen:0").(*screenDevice)
			defer d.RestoreDesktopSize()
			d.SetVideoCodec(codec)
			for _, value := range []string{"0.5", "0.75@30"} {
				if err := ConfigureMode(d, value); err != nil {
					t.Fatal(err)
				}
				mode, _ := ParseMode(value)
				if err := d.OpenWithFormat(0, 0, int(mode.FPS)); err != nil {
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
						if frame == nil {
							time.Sleep(5 * time.Millisecond)
							continue
						}
						defer frame.Release()
						w, h, err := ScaledResolution(frame.SourceWidth, frame.SourceHeight, mode.Scale)
						if err != nil || frame.Width != w || frame.Height != h {
							t.Fatalf("%s: got %dx%d, physical=%dx%d expected=%dx%d err=%v", value, frame.Width, frame.Height, frame.SourceWidth, frame.SourceHeight, w, h, err)
						}
						t.Logf("%s: physical %dx%d -> %dx%d", value, frame.SourceWidth, frame.SourceHeight, w, h)
						return
					}
					t.Fatal("no scaled frame")
				}()
			}
		})
	}
}
