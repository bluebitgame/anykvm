//go:build darwin && cgo

package capture

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeScreenDamage(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "native-screen-damage")
	args := []string{"clang", "-fblocks", "-Werror=implicit-function-declaration", "-o", executable, "testdata/native_screen_damage.m"}
	for _, framework := range []string{"ScreenCaptureKit", "VideoToolbox", "CoreMedia", "CoreVideo", "CoreGraphics", "CoreImage", "AppKit"} {
		args = append(args, "-framework", framework)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "xcrun", args...).CombinedOutput(); err != nil {
		t.Fatalf("compile damage fixture: %v\n%s", err, output)
	}
	if output, err := exec.CommandContext(ctx, executable).CombinedOutput(); err != nil {
		t.Fatalf("damage metadata: %v\n%s", err, output)
	}
}
