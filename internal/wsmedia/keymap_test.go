package wsmedia

import (
	"io/fs"
	"testing"
)

func TestHIDForCode(t *testing.T) {
	cases := map[string]uint8{
		"KeyA": 0x04, "Digit1": 0x1E, "Enter": 0x28, "Space": 0x2C,
		"ArrowUp": 0x52, "F1": 0x3A, "ShiftLeft": 0xE1, "MetaRight": 0xE7,
		"Numpad0": 0x62, "NumpadEnter": 0x58,
	}
	for code, want := range cases {
		got, ok := hidForCode(code)
		if !ok || got != want {
			t.Errorf("hidForCode(%q) = 0x%02X,%v; want 0x%02X", code, got, ok, want)
		}
	}
	if _, ok := hidForCode("NotAKey"); ok {
		t.Error("hidForCode(NotAKey) should be false")
	}
}

func TestWebAssetsEmbedded(t *testing.T) {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"index.html", "app.js", "fullscreen.js", "h264.js", "presenter.js", "timed-video.js", "keyboard.js", "pointer.js", "vendor/hls-1.7.3.min.js"} {
		b, err := fs.ReadFile(sub, name)
		if err != nil {
			t.Fatalf("embedded asset %s: %v", name, err)
		}
		if len(b) == 0 {
			t.Fatalf("embedded asset %s is empty", name)
		}
	}
}
