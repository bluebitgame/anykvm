package scrcpy

import (
	"slices"
	"strings"
	"testing"

	"anykvm/internal/capture"
)

func TestIndependentDesktopRequiresConfiguredDimensions(t *testing.T) {
	for _, tc := range []struct {
		mode, display string
	}{
		{"", ""}, {"@30", ""}, {"auto", ""}, {"auto@30", ""},
		{"1920x1080", "new_display=1920x1080"},
		{"1920x1080@30", "new_display=1920x1080"},
		{"720x1280@60", "new_display=720x1280"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			sess := NewSession(Options{})
			dev := NewCapture(sess)
			if err := capture.ConfigureMode(dev, tc.mode); err != nil {
				t.Fatal(err)
			}
			check := func() {
				t.Helper()
				// Client-negotiated sizes must neither enable nor resize a desktop.
				args := sess.serverArgs("5.0.1", "12345678", 640, 30)
				var display string
				for _, arg := range args {
					if strings.HasPrefix(arg, "new_display=") {
						display = arg
					}
				}
				if display != tc.display {
					t.Fatalf("mode=%q display=%q want=%q", tc.mode, display, tc.display)
				}
				if slices.Contains(args, "max_size=640") != (tc.display == "") {
					t.Fatalf("virtual display must keep its dimensions: %v", args)
				}
				if slices.Contains(args, "flex_display=true") {
					t.Fatal("fixed desktop must not follow client window size")
				}
			}
			check()
			dev.SetVideoStreamOptions(&capture.VideoStreamOptions{Codec: "hevc", Width: 3840, Height: 2160})
			dev.SetGameStreamCaptureOptions(&capture.VideoStreamOptions{Width: 3840, Height: 2160})
			check()
			dev.Close()
			check() // A reopen keeps the target's configured desktop dimensions.
			if err := capture.ConfigureMode(dev, "@30"); err != nil {
				t.Fatal(err)
			}
			if sess.displayWidth != 0 || sess.displayHeight != 0 {
				t.Fatal("removing the configured dimensions did not restore mirroring")
			}
		})
	}
}

func TestIndependentDesktopRejectsInvalidMode(t *testing.T) {
	sess := NewSession(Options{})
	dev := NewCapture(sess)
	for _, mode := range []string{"0x1080", "1920x0", "1920x1080 audio=true", "1920x1080:nv12"} {
		if err := capture.ConfigureMode(dev, mode); err == nil {
			t.Fatalf("accepted mode %q", mode)
		}
	}
	sess.running = true
	if err := dev.SetCaptureMode("1920x1080"); err == nil {
		t.Fatal("changed desktop mode while capture was running")
	}
}
