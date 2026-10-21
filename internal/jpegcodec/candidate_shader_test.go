package jpegcodec

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestJPEGCompareCandidateShader(t *testing.T) {
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
	mapping := source[strings.Index(source, "uint3 compareCandidateGroup("):strings.Index(source, "// ANYKVM_TILE_COMPARE")]
	common, err := os.ReadFile("../gpuconv/shaders/tile_compare.hlsl")
	if err != nil {
		t.Fatal(err)
	}
	source = gate + mapping + string(common)
	source = regexp.MustCompile(`\[numthreads\([^\]]+\)\]`).ReplaceAllString(source, "")
	source = strings.NewReplacer("groupshared uint groupDirty", "std::atomic<uint> groupDirty", " : SV_GroupID", "", " : SV_GroupIndex", "").Replace(source)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "candidate_shader.hpp"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(dir, "candidate-shader")
	cmd := exec.Command(compiler, "-std=c++17", "-O2", "-pthread", "-I"+dir, "testdata/candidate_shader.cpp", "-o", program)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile candidate comparison: %v\n%s", err, out)
	}
	if out, err := exec.Command(program).CombinedOutput(); err != nil {
		t.Fatalf("candidate comparison: %v\n%s", err, out)
	}
}
