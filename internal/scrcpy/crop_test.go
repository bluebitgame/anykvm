package scrcpy

import (
	"strings"
	"testing"

	"anykvm/internal/capture"
)

func TestCropValidation(t *testing.T) {
	for _, value := range []string{"", "1080:1920:0:240", "1272:2261:0:511", "1260:2240:6:532", "1:1:0:0", "2147483646:1:1:0"} {
		if err := ValidateCrop(value); err != nil {
			t.Errorf("rejected valid crop %q: %v", value, err)
		}
	}
	for _, value := range []string{
		"1080x1920", "1080:1920:0", "1080:1920:0:240:0", "1080:1920::240",
		"0:1920:0:0", "1080:0:0:0", "-1080:1920:0:0", "1080:1920:-1:0", "1080:1920:0:-1",
		"1080:1920:0:2.5", "1080:1920:0:240 audio=true", "1080:1920:0:240;echo bad",
		"1080:1920:0:$(echo 240)", "1080:1920:0: 240", "1080:1920:0:240\n",
		"2147483648:1:0:0", "2147483647:1:1:0", "1:2147483647:0:1",
	} {
		if err := ValidateCrop(value); err == nil {
			t.Errorf("accepted invalid crop %q", value)
		}
	}
}

func TestCropServerArgsAcrossConsumers(t *testing.T) {
	for _, crop := range []string{"", "1272:2261:0:511"} {
		t.Run(crop, func(t *testing.T) {
			sess := NewSession(Options{Crop: crop, GameStreamOrientation: "270"})
			dev := NewCapture(sess)
			check := func() {
				t.Helper()
				args := sess.serverArgs("3.3.4", "12345678", 1280, 30)
				var cropArgs []string
				for _, arg := range args {
					if strings.HasPrefix(arg, "crop=") {
						cropArgs = append(cropArgs, arg)
					}
				}
				if crop == "" {
					if len(cropArgs) != 0 {
						t.Fatalf("full display must omit crop: %v", cropArgs)
					}
				} else if len(cropArgs) != 1 || cropArgs[0] != "crop="+crop {
					t.Fatalf("crop was lost or changed: %v", cropArgs)
				}
			}
			check() // Ordinary Web/VNC capture also uses the crop.
			dev.SetGameStreamCaptureOptions(&capture.VideoStreamOptions{Bitrate: 4000})
			check()
			if err := dev.Close(); err != nil {
				t.Fatal(err)
			}
			check()
			dev.SetGameStreamCaptureOptions(nil)
			check()
		})
	}
}

func TestInvalidCropRejectedBeforeADB(t *testing.T) {
	sess := NewSession(Options{Crop: "1080:1920:0:-1"})
	if err := sess.Start(1280, 30); err == nil || !strings.Contains(err.Error(), "scrcpy.crop") {
		t.Fatalf("crop must be rejected before ADB setup: %v", err)
	}
}
