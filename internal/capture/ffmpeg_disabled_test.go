//go:build !ffmpeg || !cgo

package capture

import (
	"anykvm/internal/avmedia"
	"errors"
	"strings"
	"testing"
)

func TestDisabledFFmpegCaptureBackends(t *testing.T) {
	if _, available := ResolveFFmpeg(); available {
		t.Fatal("FFmpeg reported available in a native-only build")
	}
	video := &ffmpegDevice{argsFunc: func(int, int, int) []string {
		t.Fatal("disabled capture enumerated or configured a platform device")
		return nil
	}}
	for name, open := range map[string]func() error{
		"video": video.Open,
		"audio": NewFFmpegAudioDevice("0").Open,
	} {
		t.Run(name, func(t *testing.T) {
			err := open()
			if !errors.Is(err, avmedia.ErrUnavailable) || !strings.Contains(err.Error(), "-tags ffmpeg") {
				t.Fatalf("expected a build-tag diagnostic, got %v", err)
			}
		})
	}
}
