//go:build (linux || darwin) && cgo

package capture

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestKMSDriverModel(t *testing.T) {
	compiler := os.Getenv("CC")
	if compiler == "" {
		compiler = "cc"
	}
	args := strings.Fields(compiler)
	binary := filepath.Join(t.TempDir(), "kms-model")
	command := exec.Command(args[0], append(args[1:], "-Wall", "-Wextra", "-Werror", "testdata/kms_driver_linux.c", "-o", binary)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("KMS driver fixture: %v\n%s", err, output)
	}
	if output, err := exec.Command(binary).CombinedOutput(); err != nil {
		t.Fatalf("KMS driver model: %v\n%s", err, output)
	}
}
