package capture

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsDesktopDamageMetadata(t *testing.T) {
	compiler, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("C compiler unavailable")
	}
	raw, err := os.ReadFile("native_windows.c")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	source = source[strings.Index(source, "static int damage_add("):strings.Index(source, "bk_win_frame *bk_win_next(")]
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "desktop_damage.inc"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(dir, "desktop-damage")
	cmd := exec.Command(compiler, "-std=c11", "-O2", "-I"+dir, "testdata/desktop_damage.c", "-o", program)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile metadata reader: %v\n%s", err, out)
	}
	if out, err := exec.Command(program).CombinedOutput(); err != nil {
		t.Fatalf("metadata reader: %v\n%s", err, out)
	}
}
