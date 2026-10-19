//go:build darwin && cgo

package videoenc

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNativeH264SourceSizeWithoutPixelTransfer(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_NATIVE_MEDIA") == "" {
		t.Skip("set ANYKVM_TEST_NATIVE_MEDIA=1 to require hardware encoding")
	}
	executable := filepath.Join(t.TempDir(), "native-source-size")
	args := []string{"clang", "-Werror=implicit-function-declaration", "-o", executable, "testdata/native_source_size.m", "-framework", "Foundation", "-framework", "VideoToolbox", "-framework", "CoreVideo", "-framework", "CoreMedia"}
	if output, err := exec.Command("xcrun", args...).CombinedOutput(); err != nil {
		t.Fatalf("compile native source-size fixture: %v\n%s", err, output)
	}
	if output, err := exec.Command(executable).CombinedOutput(); err != nil {
		t.Fatalf("native source-size encoding: %v\n%s", err, output)
	} else {
		t.Logf("%s", output)
	}
}
