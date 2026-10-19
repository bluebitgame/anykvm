package scrcpy

import (
	"strings"
	"testing"

	"anykvm/internal/capture"
)

func TestGameStreamBitrateCeilingAndRestore(t *testing.T) {
	for _, tc := range []struct {
		name         string
		configured   int
		negotiated   int
		wantBitrate  string
		wantRestored string
	}{
		{"client below ceiling", 8000000, 4000, "4000000", "8000000"},
		{"client above ceiling", 8000000, 12000, "8000000", "8000000"},
		{"client matches ceiling", 8000000, 8000, "8000000", "8000000"},
		{"no configured ceiling", 0, 12000, "12000000", ""},
		{"no negotiated bitrate", 8000000, 0, "8000000", "8000000"},
		{"server default", 0, 0, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := NewSession(Options{BitRate: tc.configured})
			dev := NewCapture(sess)
			check := func(want string) {
				t.Helper()
				sess.mu.Lock()
				args := sess.serverArgs("3.3.4", "12345678", 0, 30)
				sess.mu.Unlock()
				bitrate, fps, maxSize := "", "", ""
				for _, arg := range args {
					if strings.HasPrefix(arg, "video_bit_rate=") {
						bitrate = strings.TrimPrefix(arg, "video_bit_rate=")
					}
					if strings.HasPrefix(arg, "max_fps=") {
						fps = strings.TrimPrefix(arg, "max_fps=")
					}
					if strings.HasPrefix(arg, "max_size=") {
						maxSize = arg
					}
				}
				if bitrate != want || fps != "30" || maxSize != "" {
					t.Fatalf("Android args: bitrate=%q fps=%q size=%q; want bitrate=%q, 30 FPS, source dimensions", bitrate, fps, maxSize, want)
				}
			}
			check(tc.wantRestored)
			profile := &capture.VideoStreamOptions{Bitrate: tc.negotiated, FPS: 60}
			dev.SetGameStreamCaptureOptions(profile)
			check(tc.wantBitrate)
			if err := dev.Close(); err != nil {
				t.Fatal(err)
			}
			check(tc.wantRestored)
			dev.SetGameStreamCaptureOptions(profile) // IDR restart reuses the same budget.
			check(tc.wantBitrate)
			dev.SetGameStreamCaptureOptions(nil) // Web/VNC takeover restores the configuration.
			check(tc.wantRestored)
		})
	}
}
