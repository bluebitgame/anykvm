//go:build cgo && (linux || darwin)

package videoenc

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNativeV4L2DriverModel(t *testing.T) {
	args := []string{"-std=c11", "-D_POSIX_C_SOURCE=200809L", "-Wall", "-Wextra", "-Werror"}
	if runtime.GOOS == "darwin" {
		headers := os.Getenv("ANYKVM_TEST_V4L2_HEADERS")
		if headers == "" {
			t.Skip("set ANYKVM_TEST_V4L2_HEADERS to a Linux sysroot's usr/include on macOS")
		}
		args = append(args, "-idirafter", headers, "-idirafter", filepath.Join(headers, "x86_64-linux-gnu"))
	}
	executable := filepath.Join(t.TempDir(), "native-v4l2")
	args = append(args, "testdata/native_v4l2.c", "-o", executable)
	if output, err := exec.Command("cc", args...).CombinedOutput(); err != nil {
		t.Fatalf("compile V4L2 fixture: %v\n%s", err, output)
	}
	if output, err := exec.Command(executable).CombinedOutput(); err != nil {
		t.Fatalf("V4L2 driver model: %v\n%s", err, output)
	}
}
