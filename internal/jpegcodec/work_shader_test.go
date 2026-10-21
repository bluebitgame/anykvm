package jpegcodec

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Execute the actual append and indirect-argument HLSL on CPU with concurrent
// tile groups and checked buffers. No GPU hardware is required.
func TestJPEGWorkShader(t *testing.T) {
	compiler, err := exec.LookPath("c++")
	if err != nil {
		t.Skip("C++ compiler unavailable")
	}
	shader, err := os.ReadFile("shaders/jpeg420.hlsl")
	if err != nil {
		t.Fatal(err)
	}
	source := string(shader)
	gate := source[strings.Index(source, "// MCU tile gate."):strings.Index(source, "// Keep coefficient blocks")]
	appendWork := source[strings.Index(source, "void compareFlag("):strings.Index(source, "// JPEG solid comparison hooks.")]
	prepare := source[strings.Index(source, "void prepareWork("):strings.Index(source, "float lumaSample(")]
	source = source[:strings.Index(source, "#ifdef __spirv__")] + gate + appendWork + prepare
	source = strings.ReplaceAll(source, " : SV_DispatchThreadID", "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "work_shader.hpp"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(dir, "work-shader")
	cmd := exec.Command(compiler, "-std=c++17", "-O2", "-pthread", "-I"+dir, "testdata/work_shader.cpp", "-o", program)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile work shader: %v\n%s", err, output)
	}
	if output, err := exec.Command(program).CombinedOutput(); err != nil {
		t.Fatalf("work shader: %v\n%s", err, output)
	}
}
