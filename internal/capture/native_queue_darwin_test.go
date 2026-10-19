//go:build darwin && cgo

package capture

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNativeFrameQueueCloseAndReopen(t *testing.T) {
	// Compile a native fixture against the production Objective-C queue. This
	// creates CoreMedia samples only; it never enumerates or opens any camera.
	executable := filepath.Join(t.TempDir(), "native-frame-queue")
	args := []string{"clang", "-fblocks", "-Werror=implicit-function-declaration", "-o", executable,
		"capture_darwin.m", "testdata/native_frame_queue.m"}
	for _, framework := range []string{"AVFoundation", "CoreMedia", "CoreMediaIO", "CoreGraphics", "CoreVideo", "Cocoa", "ImageIO", "CoreServices", "CoreImage"} {
		args = append(args, "-framework", framework)
	}
	if output, err := exec.Command("xcrun", args...).CombinedOutput(); err != nil {
		t.Fatalf("compile native fixture: %v\n%s", err, output)
	}
	if output, err := exec.Command(executable).CombinedOutput(); err != nil {
		t.Fatalf("queue close/reopen: %v\n%s", err, output)
	}
}
