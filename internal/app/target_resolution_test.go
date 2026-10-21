package app

import (
	"anykvm/internal/capture"
	"testing"
)

func TestInitialTargetResolution(t *testing.T) {
	previous := targetScreenSize
	defer func() { targetScreenSize = previous }()
	targetScreenSize = func(source string) (int, int) {
		if source == "screen:0" {
			return 2880, 1800
		}
		return 0, 0
	}
	for _, tc := range []struct {
		value string
		w, h  int
	}{
		{"", 2880, 1800}, {"@30", 2880, 1800}, {"1.0", 2880, 1800},
		{"0.75", 2160, 1350}, {"0.5@30", 1440, 900}, {"1920x1080", 1920, 1080},
	} {
		mode, err := capture.ParseMode(tc.value)
		if err != nil {
			t.Fatal(err)
		}
		w, h := initialTargetSize("screen:0", mode)
		if w != tc.w || h != tc.h {
			t.Fatalf("%q initialized %dx%d, want %dx%d", tc.value, w, h, tc.w, tc.h)
		}
	}
	mode, _ := capture.ParseMode("")
	if mode.Width != 0 || mode.Height != 0 || mode.Scale != 1 {
		t.Fatalf("native default: %+v", mode)
	}
	if w, h := initialTargetSize("offline", mode); w <= 0 || h <= 0 {
		t.Fatal("offline source produced an invalid protocol canvas")
	}
	// Explicit dimensions take precedence even if a caller supplies a scale.
	if w, h := initialTargetSize("screen:0", capture.Mode{Width: 1920, Height: 1080, Scale: .5}); w != 1920 || h != 1080 {
		t.Fatalf("explicit size scaled: %dx%d", w, h)
	}
}
