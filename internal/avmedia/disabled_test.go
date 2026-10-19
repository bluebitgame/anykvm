//go:build !ffmpeg || !cgo

package avmedia

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestDisabledBackendNeverLoadsLibraries(t *testing.T) {
	t.Setenv("ANYKVM_FFMPEG_LIBDIR", t.TempDir())
	if Enabled {
		t.Fatal("FFmpeg backend included without both its build tag and CGO")
	}
	for name, open := range map[string]func() error{
		"availability":     Available,
		"URL availability": URLMediaAvailable,
		"video encoder":    func() error { _, err := NewEncoder(VideoOptions{Codec: "libx264"}); return err },
		"audio encoder":    func() error { _, err := NewAudioEncoder(10, 64); return err },
		"video decoder":    func() error { _, err := NewDecoder("h264"); return err },
		"audio decoder":    func() error { _, err := NewAudioDecoder("aac", nil, 48000, 2); return err },
		"capture":          func() error { _, err := OpenSource("v4l2", "/dev/video0", nil, false); return err },
		"video capture":    func() error { _, err := OpenVideoSource("dshow", "video=0", nil, "h264"); return err },
		"URL media": func() error {
			_, err := OpenMedia(context.Background(), "https://example.invalid/movie.mp4")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := open()
			if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "-tags ffmpeg") {
				t.Fatalf("expected a build-tag diagnostic, got %v", err)
			}
		})
	}
	if logs := DeviceLog("dshow"); len(logs) != 0 {
		t.Fatal("disabled backend enumerated FFmpeg devices")
	}
}
