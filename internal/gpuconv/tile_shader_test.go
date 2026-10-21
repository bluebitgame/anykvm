package gpuconv

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestSharedTileCompareShader(t *testing.T) {
	compiler, err := exec.LookPath("c++")
	if err != nil {
		t.Skip("C++ compiler unavailable")
	}
	dir := t.TempDir()
	source := regexp.MustCompile(`\[numthreads\([^\]]+\)\]`).ReplaceAllString(tileCompareShader, "")
	source = strings.NewReplacer("groupshared uint groupDirty", "std::atomic<uint> groupDirty", " : SV_GroupID", "", " : SV_GroupIndex", "").Replace(source)
	if err := os.WriteFile(filepath.Join(dir, "tile_compare.hpp"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(dir, "tile-compare")
	if out, err := exec.Command(compiler, "-std=c++17", "-O2", "-pthread", "-I"+dir, "testdata/tile_compare.cpp", "-o", program).CombinedOutput(); err != nil {
		t.Fatalf("compile shared GPU kernel: %v\n%s", err, out)
	}
	if out, err := exec.Command(program).CombinedOutput(); err != nil {
		t.Fatalf("execute shared GPU kernel: %v\n%s", err, out)
	}
}
