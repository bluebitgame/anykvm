//go:build linux && cgo

package gpuconv

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestJPEGBufferReuse(t *testing.T) {
	compiler, err := exec.Command("go", "env", "CC").Output()
	if err != nil {
		t.Fatal(err)
	}
	command := strings.Fields(strings.TrimSpace(string(compiler)))
	if len(command) == 0 {
		t.Fatal("missing C compiler")
	}
	binary := filepath.Join(t.TempDir(), "jpeg-buffers")
	args := append(command[1:], "testdata/jpeg_buffers.c", "-O2", "-Ithird_party", "-ldl", "-lm", "-o", binary)
	if output, err := exec.Command(command[0], args...).CombinedOutput(); err != nil {
		t.Fatalf("build buffer fixture: %v\n%s", err, output)
	}
	if output, err := exec.Command(binary).CombinedOutput(); err != nil {
		t.Fatalf("buffer reuse: %v\n%s", err, output)
	}
}
