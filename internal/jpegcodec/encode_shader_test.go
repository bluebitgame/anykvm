package jpegcodec

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"math/rand"
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
	source = source[strings.Index(source, "// Shared DCT workspace."):strings.Index(source, "// One restart interval")]
	begin, end := strings.Index(source, "uint2 sourcePosition("), strings.Index(source, "float lumaSample(")
	gate := string(shader)[strings.Index(string(shader), "// MCU tile gate."):strings.Index(string(shader), "// Shared DCT workspace.")]
	source = constants + gate + source[:begin] + source[end:]
	source = regexp.MustCompile(`\[numthreads\([^\]]+\)\]`).ReplaceAllString(source, "")
	source = strings.NewReplacer("groupshared ", "", "[unroll]", "", "void main(", "void transformMain(",
		"inout uint lowMask", "uint &lowMask", "inout uint highMask", "uint &highMask",
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
	cases := []struct {
		name          string
		width, height int
		pixels        []byte
	}{
		{"edge", 1, 1, colorFrame(1, 1)},
		{"gradient", 35, 19, colorFrame(35, 19)},
		{"dirty-edge", 65, 17, colorFrame(65, 17)},
		{"solid", 35, 19, bytes.Repeat([]byte{128, 128, 128, 255}, 35*19)},
		{"near-flat", 35, 19, make([]byte, 35*19*4)},
		{"noise", 35, 19, make([]byte, 35*19*4)},
		{"white", 1, 1, []byte{255, 255, 255, 255}},
		{"blue", 1, 1, []byte{255, 0, 0, 255}},
	}
	for i := 0; i < len(cases[4].pixels); i += 4 {
		value := byte(127 + (i/4)%2)
		copy(cases[4].pixels[i:i+4], []byte{value, value, value, 255})
	}
	rand.New(rand.NewSource(41)).Read(cases[5].pixels)
	for _, fixture := range cases {
		for _, quality := range []int{1, 80, 100} {
			w, h := fixture.width, fixture.height
			quant := quantization420(quality)
			var input bytes.Buffer
			binary.Write(&input, binary.LittleEndian, uint32(w))
			binary.Write(&input, binary.LittleEndian, uint32(h))
			binary.Write(&input, binary.LittleEndian, quant)
			input.Write(fixture.pixels)
			command := exec.Command(program)
			command.Stdin = &input
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("shader fixture: %v\n%s", err, output)
			}
			want := referenceCoefficients(fixture.pixels, w, h, 0, w, h, quality)
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
					t.Fatalf("%s %dx%d quality %d coefficient %d: got %d want %d", fixture.name, w, h, quality, i, got[i], v)
				}
			}
			// Decode the actual shader coefficients using an independent JPEG
			// decoder, and compare pixels with the double-precision DCT oracle.
			decode := func(coefficients []int32) *image.YCbCr {
				t.Helper()
				data, err := encodeQuantized420Restart(coefficients, w, h, quality, true)
				if err != nil {
					t.Fatal(err)
				}
				decoded, err := jpeg.Decode(bytes.NewReader(data))
				if err != nil {
					t.Fatal(err)
				}
				return decoded.(*image.YCbCr)
			}
			actual, expected := decode(got), decode(want)
			if actual.Bounds() != image.Rect(0, 0, w, h) {
				t.Fatal("JPEG dimensions changed")
			}
			var difference float64
			for y := range h {
				for x := range w {
					a := color.NRGBAModel.Convert(actual.At(x, y)).(color.NRGBA)
					b := color.NRGBAModel.Convert(expected.At(x, y)).(color.NRGBA)
					difference += math.Abs(float64(a.R)-float64(b.R)) + math.Abs(float64(a.G)-float64(b.G)) + math.Abs(float64(a.B)-float64(b.B))
				}
			}
			if mean := difference / float64(w*h*3); mean > 2 {
				t.Fatalf("%s %dx%d quality %d: decoded color difference %.3f", fixture.name, w, h, quality, mean)
			}
		}
	}
}
