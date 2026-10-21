package jpegcodec

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestJPEGIndexShader(t *testing.T) {
	compiler, err := exec.LookPath("c++")
	if err != nil {
		t.Skip("C++ compiler unavailable")
	}
	raw, err := os.ReadFile("shaders/jpeg420.hlsl")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	constants := source[:strings.Index(source, "#ifdef __spirv__")]
	gate := source[strings.Index(source, "// MCU tile gate."):strings.Index(source, "// Keep coefficient blocks")]
	sizes := source[strings.Index(source, "// One restart interval"):strings.Index(source, "struct EntropyWriter")]
	index := source[strings.Index(source, "// segmentInfo:"):strings.Index(source, "uint jpegWorkOffset(")]
	source = constants + gate + sizes + index
	source = regexp.MustCompile(`\[numthreads\([^\]]+\)\]`).ReplaceAllString(source, "")
	source = strings.NewReplacer("groupshared ", "", " : SV_GroupID", "", " : SV_GroupThreadID", "", " : SV_DispatchThreadID", "").Replace(source)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index_shader.hpp"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(dir, "index-shader")
	command := exec.Command(compiler, "-std=c++17", "-O2", "-pthread", "-I"+dir, "testdata/index_shader.cpp", "-o", program)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile index shader: %v\n%s", err, output)
	}
	if output, err := exec.Command(program).CombinedOutput(); err != nil {
		t.Fatalf("index shader: %v\n%s", err, output)
	}
}
