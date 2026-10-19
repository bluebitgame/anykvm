//go:build darwin && cgo

package videoenc

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNativeAnnexBContiguousAndSegmentedBlocks(t *testing.T) {
	// Synthetic CoreMedia buffers exercise the production converter without
	// camera permissions, a phone, or a hardware encoder session.
	executable := filepath.Join(t.TempDir(), "native-annexb")
	args := []string{"clang", "-Werror=implicit-function-declaration", "-o", executable, "testdata/native_annexb.m", "-framework", "Foundation", "-framework", "CoreMedia"}
	if output, err := exec.Command("xcrun", args...).CombinedOutput(); err != nil {
		t.Fatalf("compile Annex-B fixture: %v\n%s", err, output)
	}
	if output, err := exec.Command(executable).CombinedOutput(); err != nil {
		t.Fatalf("Annex-B conversion: %v\n%s", err, output)
	}
}
