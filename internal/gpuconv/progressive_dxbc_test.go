package gpuconv

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the native loader without a GPU or D3DCompiler. Both source files
// contain a main entry point, so selecting the source's table matters.
func TestProgressivePrecompiledShaders(t *testing.T) {
	compiler, err := exec.LookPath("c++")
	if err != nil {
		t.Skip("C++ compiler unavailable")
	}
	raw, err := os.ReadFile("progressive_windows.c")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	start := strings.Index(source, "#ifdef BK_PROGRESSIVE_PRECOMPILED")
	end := strings.Index(source, "bk_d3d_progressive *bk_d3d_progressive_new(")
	if start < 0 || end <= start {
		t.Fatal("precompiled Progressive shader loader not found")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "progressive_precompiled.hpp"), []byte(source[start:end]), 0600); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []struct {
		name, prefix, kind string
		entries            []string
	}{
		{"progressive", "bk_progressive", "encoder", []string{"main", "encodeTiles", "indexSegments", "packSegments"}},
		{"progressive_surface", "bk_progressive_surface", "surface", []string{"main"}},
	} {
		var header strings.Builder
		fmt.Fprintf(&header, "typedef struct { const char *entry; const unsigned char *bytes; size_t size; } %s_dxbc_stage;\n", stage.prefix)
		fmt.Fprintf(&header, "static const %s_dxbc_stage %s_dxbc_stages[] = {\n", stage.prefix, stage.prefix)
		for _, entry := range stage.entries {
			code := "DXBC:" + stage.kind + ":" + entry
			fmt.Fprintf(&header, "{\"%s\", (const unsigned char *)\"%s\", %d},\n", entry, code, len(code))
		}
		header.WriteString("};\n")
		if err := os.WriteFile(filepath.Join(dir, stage.name+"_dxbc.h"), []byte(header.String()), 0600); err != nil {
			t.Fatal(err)
		}
	}
	program := filepath.Join(dir, "progressive-dxbc")
	command := exec.Command(compiler, "-std=c++17", "-O2", "-DBK_PROGRESSIVE_PRECOMPILED", "-I"+dir,
		"testdata/progressive_dxbc.cpp", "-o", program)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile Progressive DXBC fixture: %v\n%s", err, output)
	}
	if output, err := exec.Command(program).CombinedOutput(); err != nil {
		t.Fatalf("Progressive DXBC loader: %v\n%s", err, output)
	}
}
