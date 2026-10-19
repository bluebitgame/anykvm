//go:build darwin && cgo

package capture

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNativeDeferredClipboardReaderLifetime(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "native-clipboard-files")
	args := []string{"clang", "-fblocks", "-Wall", "-Wextra", "-Werror", "-o", executable,
		"testdata/native_clipboard_files.m", "-framework", "AppKit"}
	if output, err := exec.Command("xcrun", args...).CombinedOutput(); err != nil {
		t.Fatalf("compile native clipboard fixture: %v\n%s", err, output)
	}
	// Directly exercise the production presenter and lease without touching
	// the user's pasteboard, Finder, or macOS automation permissions.
	if output, err := exec.Command(executable).CombinedOutput(); err != nil {
		t.Fatalf("native clipboard reader lifetime: %v\n%s", err, output)
	}
}
