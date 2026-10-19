//go:build darwin && cgo

package capture

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeScreenColorRange(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "native-screen-color")
	args := []string{"clang", "-fblocks", "-Werror=implicit-function-declaration", "-o", executable, "testdata/native_screen_color.m"}
	for _, framework := range []string{"ScreenCaptureKit", "VideoToolbox", "CoreMedia", "CoreVideo", "CoreGraphics", "CoreImage", "AppKit"} {
		args = append(args, "-framework", framework)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "xcrun", args...).CombinedOutput(); err != nil {
		t.Fatalf("compile color fixture: %v\n%s", err, output)
	}
	if output, err := exec.CommandContext(ctx, executable).CombinedOutput(); err != nil {
		t.Fatalf("color policy: %v\n%s", err, output)
	}
	if os.Getenv("ANYKVM_TEST_NATIVE_MEDIA") != "" {
		output, err := exec.CommandContext(ctx, executable, "--hardware").CombinedOutput()
		if err != nil {
			t.Fatalf("hardware color round trip: %v\n%s", err, output)
		}
		t.Log(string(output))
	}
}
