//go:build windows && cgo

package jpegcodec

import (
	"bytes"
	"encoding/binary"
	"image/color"
	"image/jpeg"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Synthetic keyed-mutex textures exercise the production GPU pipeline. A second
// mode uploads known coefficients to check Huffman/packing byte for byte.
func TestD3D11JPEGShader(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_WINDOWS_GPU_JPEG") == "" {
		t.Skip("set ANYKVM_TEST_WINDOWS_GPU_JPEG=1 on a Windows GPU host")
	}
	cc, err := exec.Command("go", "env", "CC").Output()
	if err != nil {
		t.Fatal(err)
	}
	compiler := strings.TrimSpace(string(cc))
	words := strings.Fields(compiler)
	if _, err := exec.LookPath(strings.Trim(compiler, "\"")); err == nil {
		words = []string{strings.Trim(compiler, "\"")}
	}
	if len(words) == 0 {
		t.Fatal("missing C compiler")
	}
	dir := t.TempDir()
	executable := filepath.Join(dir, "jpeg-fixture.exe")
	arguments := append(words[1:], "testdata/d3d11_jpeg_fixture.c", "-O2", "-ld3d11", "-ldxgi", "-ldxguid", "-o", executable)
	if out, err := exec.Command(words[0], arguments...).CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	shader := filepath.Join(dir, "jpeg.hlsl")
	if err := os.WriteFile(shader, []byte(jpegComputeShader), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(geometry [5]int, quality int, payload []byte, coefficientsOnly bool) []byte {
		t.Helper()
		var input bytes.Buffer
		var dimensions [5]uint32
		for i, value := range geometry {
			dimensions[i] = uint32(value)
		}
		var parameters jpegFrameParameters
		if err := parameters.prepare(geometry[3], geometry[4], quality); err != nil {
			t.Fatal(err)
		}
		header := parameters.header
		binary.Write(&input, binary.LittleEndian, dimensions)
		binary.Write(&input, binary.LittleEndian, parameters.quantization)
		binary.Write(&input, binary.LittleEndian, jpegPackedEntropyCodes)
		binary.Write(&input, binary.LittleEndian, uint32(len(header)))
		input.Write(header)
		input.Write(payload)
		args := []string{shader}
		if coefficientsOnly {
			args = append(args, "coefficients")
		} else if geometry == ([5]int{129, 131, 0, 129, 131}) {
			args = append(args, "incremental")
		}
		command := exec.Command(executable, args...)
		command.Stdin = &input
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, err := command.Output()
		if err != nil {
			t.Fatalf("geometry=%v quality=%d: %v\n%s", geometry, quality, err, stderr.String())
		}
		return output
	}
	for _, geometry := range [][5]int{{1, 1, 0, 1, 1}, {19, 17, 0, 19, 17}, {40, 32, 0, 21, 15}, {19, 17, 90, 17, 19}, {19, 17, 180, 19, 17}, {19, 17, 270, 17, 19}, {129, 131, 0, 129, 131}} {
		for _, quality := range []int{20, 80, 100} {
			sw, sh, rotation, width, height := geometry[0], geometry[1], geometry[2], geometry[3], geometry[4]
			pixels := jpegTestPixels(sw, sh)
			if rotation == 180 {
				rand.New(rand.NewSource(710)).Read(pixels)
			}
			actual := run(geometry, quality, pixels, false)
			expected, err := encodeQuantized420Restart(referenceCoefficients(pixels, sw, sh, rotation, width, height, quality), width, height, quality, true)
			if err != nil {
				t.Fatal(err)
			}
			got, err := jpeg.Decode(bytes.NewReader(actual))
			if err != nil {
				t.Fatal(err)
			}
			want, err := jpeg.Decode(bytes.NewReader(expected))
			if err != nil {
				t.Fatal(err)
			}
			if got.Bounds() != want.Bounds() {
				t.Fatal("GPU JPEG dimensions changed")
			}
			difference := 0.0
			for y := range height {
				for x := range width {
					a := color.NRGBAModel.Convert(got.At(x, y)).(color.NRGBA)
					b := color.NRGBAModel.Convert(want.At(x, y)).(color.NRGBA)
					difference += math.Abs(float64(a.R)-float64(b.R)) + math.Abs(float64(a.G)-float64(b.G)) + math.Abs(float64(a.B)-float64(b.B))
				}
			}
			if mean := difference / float64(width*height*3); mean > 2 {
				t.Fatalf("GPU/scalar image difference %.3f", mean)
			}
		}
	}
	for _, size := range [][2]int{{1, 1}, {32, 16}, {48, 48}, {64, 32}, {64, 48}} {
		width, height := size[0], size[1]
		coefficients := make([]int32, ((width+15)/16)*((height+15)/16)*384)
		rng := rand.New(rand.NewSource(43))
		for offset := 0; offset < len(coefficients); offset += 64 {
			if width == 1 {
				continue
			} // Golden all-zero scan.
			coefficients[offset] = int32(rng.Intn(2048) - 1024)
			for i := 1; i < 64; i++ {
				if width == 64 {
					coefficients[offset+i] = 1023
					if i%2 == 0 {
						coefficients[offset+i] = -1023
					}
				} else if rng.Intn(3) == 0 {
					coefficients[offset+i] = int32(rng.Intn(2047) - 1023)
				}
			}
		}
		var payload bytes.Buffer
		binary.Write(&payload, binary.LittleEndian, coefficients)
		actual := run([5]int{width, height, 0, width, height}, 80, payload.Bytes(), true)
		expected, err := encodeQuantized420Restart(coefficients, width, height, 80, true)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(actual, expected) {
			t.Fatalf("%dx%d GPU Huffman/packing differs from scalar oracle", width, height)
		}
	}
	if encoder, err := NewD3D11Encoder(^uint64(0)); err == nil {
		encoder.Close()
		t.Fatal("substituted another GPU for invalid capture LUID")
	}
}
