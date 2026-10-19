package capture

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWASAPINativeQueueContinuityAndLatencyBound(t *testing.T) {
	compiler, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("a C compiler is needed for the native audio queue test")
	}
	name := "wasapi-queue"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(t.TempDir(), name)
	output, err := exec.Command(compiler, "-std=c11", "-Wall", "-Wextra", "-Werror", "-I.", "testdata/native_wasapi_queue.c", "-o", binary).CombinedOutput()
	if err != nil {
		t.Fatalf("native queue compile: %v\n%s", err, output)
	}
	if output, err = exec.Command(binary).CombinedOutput(); err != nil {
		t.Fatalf("native queue continuity: %v\n%s", err, output)
	}
}
