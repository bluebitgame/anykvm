//go:build darwin && cgo

package capture

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestUSBAudioQueueAlignmentOverflowAndShutdown(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "usb-audio")
	args := []string{"clang", "-fblocks", "-Werror=implicit-function-declaration", "-o", executable, "capture_darwin.m", "testdata/native_usb_audio.m"}
	for _, framework := range []string{"AVFoundation", "CoreMedia", "CoreMediaIO", "CoreGraphics", "CoreVideo", "Cocoa", "CoreServices", "CoreImage"} {
		args = append(args, "-framework", framework)
	}
	if out, err := exec.Command("xcrun", args...).CombinedOutput(); err != nil {
		t.Fatalf("compile audio fixture: %v\n%s", err, out)
	}
	if out, err := exec.Command(executable).CombinedOutput(); err != nil {
		t.Fatalf("audio queue: %v\n%s", err, out)
	}
}
