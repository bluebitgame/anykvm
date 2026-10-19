//go:build darwin && cgo

package videoenc

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNativeLetterboxChromaBoundary(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_NATIVE_MEDIA") == "" {
		t.Skip("set ANYKVM_TEST_NATIVE_MEDIA=1 to test the system pixel transfer service")
	}
	executable := filepath.Join(t.TempDir(), "native-letterbox")
	args := []string{"clang", "-Werror=implicit-function-declaration", "-o", executable, "testdata/native_letterbox.m", "-framework", "Foundation", "-framework", "VideoToolbox", "-framework", "CoreVideo", "-framework", "CoreMedia"}
	if output, err := exec.Command("xcrun", args...).CombinedOutput(); err != nil {
		t.Fatalf("compile letterbox fixture: %v\n%s", err, output)
	}
	if output, err := exec.Command(executable).CombinedOutput(); err != nil {
		t.Fatalf("letterbox chroma boundary: %v\n%s", err, output)
	}
}
