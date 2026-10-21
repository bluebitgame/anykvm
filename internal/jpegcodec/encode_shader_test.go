package jpegcodec

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestJPEGEncodeShaderCoefficients(t *testing.T) {
	compiler, err := exec.LookPath("c++")
	if err != nil {
		t.Skip("C++ compiler unavailable")
	}
	dir := t.TempDir()
	shader, err := os.ReadFile("shaders/jpeg420.hlsl")
	if err != nil {
		t.Fatal(err)
	}
	source := string(shader)
	constants := source[:strings.Index(source, "#ifdef __spirv__")]
	source = source[strings.Index(source, "// Orthonormal DCT basis"):strings.Index(source, "// One restart interval")]
	begin, end := strings.Index(source, "uint2 sourcePosition("), strings.Index(source, "float lumaSample(")
	source = constants + source[:begin] + source[end:]
	source = regexp.MustCompile(`\[numthreads\([^\]]+\)\]`).ReplaceAllString(source, "")
	source = strings.NewReplacer("groupshared ", "", "[unroll]", "", "void main(", "void transformMain(",
		" : SV_GroupID", "", " : SV_GroupThreadID", "", "groupID.xy", "uint2(groupID.x,groupID.y)",
		"threadID.xy", "uint2(threadID.x,threadID.y)").Replace(source)
	if err := os.WriteFile(filepath.Join(dir, "encode_shader.hpp"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(dir, "encode-shader")
	command := exec.Command(compiler, "-std=c++17", "-O2", "-pthread", "-I"+dir, "testdata/encode_shader.cpp", "-o", program)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile shader fixture: %v\n%s", err, output)
	}
	for _, size := range [][2]int{{1, 1}, {35, 19}} {
		for _, quality := range []int{1, 80, 100} {
			w, h := size[0], size[1]
			quant := quantization420(quality)
			var input bytes.Buffer
			binary.Write(&input, binary.LittleEndian, uint32(w))
			binary.Write(&input, binary.LittleEndian, uint32(h))
			binary.Write(&input, binary.LittleEndian, quant)
			command := exec.Command(program)
			command.Stdin = &input
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("shader fixture: %v\n%s", err, output)
			}
			want := referenceCoefficients(colorFrame(w, h), w, h, 0, w, h, quality)
			got := make([]int32, len(want))
			if err := binary.Read(bytes.NewReader(output), binary.LittleEndian, got); err != nil {
				t.Fatal(err)
			}
			if len(output) != len(got)*4 {
				t.Fatal("unexpected shader output size")
			}
			for i, v := range want {
				delta := got[i] - v
				if delta < -1 || delta > 1 {
					t.Fatalf("size %v quality %d coefficient %d: got %d want %d", size, quality, i, got[i], v)
				}
			}
		}
	}
}
