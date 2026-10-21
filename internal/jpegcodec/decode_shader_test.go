package jpegcodec

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Run the actual shader with checked buffers on hosts without D3D11/Vulkan.
func TestJPEGDecodeShaderOutput(t *testing.T) {
	compiler, err := exec.LookPath("c++")
	if err != nil {
		t.Skip("C++ compiler unavailable")
	}
	dir := t.TempDir()
	shader, err := os.ReadFile("shaders/jpeg_decode.hlsl")
	if err != nil {
		t.Fatal(err)
	}
	source := regexp.MustCompile(`(?m)^BINDING\([^\n]+\n`).ReplaceAllString(string(shader), "")
	source = regexp.MustCompile(`\[numthreads\([^\]]+\)\]`).ReplaceAllString(source, "")
	source = strings.NewReplacer("groupshared ", "", "inout EntropyReader reader", "EntropyReader &reader",
		"(EntropyReader)0", "EntropyReader{}", "(HuffmanValue)0", "HuffmanValue{}", "void main(", "void entropyMain(",
		" : SV_DispatchThreadID", "", " : SV_GroupIndex", "", " : SV_GroupID", "", " : SV_GroupThreadID", "").Replace(source)
	if err := os.WriteFile(filepath.Join(dir, "decode_shader.hpp"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(dir, "decode-shader")
	command := exec.Command(compiler, "-std=c++17", "-O2", "-pthread", "-I"+dir, "testdata/decode_shader.cpp", "-o", program)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile shader fixture: %v\n%s", err, output)
	}
	type fixture struct {
		frame   *computeJPEG
		image   image.Image
		invalid bool
	}
	var cases []fixture
	add := func(data []byte, w, h int) {
		t.Helper()
		frame, err := parseComputeJPEG(data, w, h)
		if err != nil {
			t.Fatal(err)
		}
		img, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		cases = append(cases, fixture{frame: frame, image: img})
	}
	for _, restart := range []bool{false, true} {
		data, _ := computeDecodeFixture(t, restart)
		for _, size := range [][2]int{{36, 20}, {64, 48}, {2, 2}} {
			add(data, size[0], size[1])
		}
	}
	for _, sampling := range [][2]int{{1, 1}, {2, 1}, {1, 2}, {2, 2}} {
		data, _ := sampledComputeFixture(t, sampling[0], sampling[1])
		add(data, 20, 18)
	}
	var gray bytes.Buffer
	if err := jpeg.Encode(&gray, image.NewGray(image.Rect(0, 0, 19, 17)), nil); err != nil {
		t.Fatal(err)
	}
	add(gray.Bytes(), 20, 18)
	broken := *cases[0].frame
	broken.metadata = append([]uint32(nil), broken.metadata...)
	broken.metadata[decodeSegmentBase+1] = 1
	cases = append(cases, fixture{frame: &broken, invalid: true})
	var input bytes.Buffer
	for _, test := range cases {
		frame := test.frame
		binary.Write(&input, binary.LittleEndian, uint32(len(frame.metadata)))
		binary.Write(&input, binary.LittleEndian, uint32(len(frame.entropy)))
		binary.Write(&input, binary.LittleEndian, frame.metadata)
		input.Write(frame.entropy)
	}
	command = exec.Command(program)
	command.Stdin = &input
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("shader fixture: %v\n%s", err, stderr.Bytes())
	}
	result := bytes.NewReader(output)
	for n, test := range cases {
		var status uint32
		if err := binary.Read(result, binary.LittleEndian, &status); err != nil {
			t.Fatal(err)
		}
		if test.invalid {
			if status == 0 {
				t.Fatal("shader accepted truncated entropy")
			}
			continue
		}
		if status != 0 {
			t.Fatalf("case %d: invalid shader entropy", n)
		}
		want, err := entropyOracle(test.frame)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]int32, len(want))
		if err := binary.Read(result, binary.LittleEndian, got); err != nil {
			t.Fatal(err)
		}
		for i, v := range want {
			if got[i] != v {
				t.Fatalf("case %d coefficient %d: got %d want %d", n, i, got[i], v)
			}
		}
		w, h := int(test.frame.metadata[2]), int(test.frame.metadata[3])
		pixels := make([]byte, w*h*3/2)
		if _, err := result.Read(pixels); err != nil {
			t.Fatal(err)
		}
		reference := oracleNV12(test.image, w, h)
		for i, v := range reference {
			delta := int(pixels[i]) - int(v)
			if delta < -4 || delta > 4 {
				t.Fatalf("case %d NV12 byte %d: got %d want %d", n, i, pixels[i], v)
			}
		}
	}
	if result.Len() != 0 {
		t.Fatal("unexpected shader fixture output")
	}
}
