//go:build cgo && (darwin || linux || windows)

package rdp

import (
	"bytes"
	"encoding/binary"
	"io"
	"math/rand"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestProgressiveNativeEntropy(t *testing.T) {
	compiler, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("C compiler unavailable")
	}
	program := filepath.Join(t.TempDir(), "progressive-entropy")
	command := exec.Command(compiler, "-std=c11", "-O3", "testdata/progressive_entropy.c", "progressive_avx2.c", "-o", program)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile entropy fixture: %v\n%s", err, output)
	}
	var inputs bytes.Buffer
	var expected [][]byte
	rng := rand.New(rand.NewSource(7351))
	// Cover every reservoir alignment, including zero- and 32-bit appends.
	for prefix := 0; prefix < 64; prefix++ {
		for count := 0; count <= 32; count++ {
			pairs := [][2]uint32{{rng.Uint32(), uint32(min(prefix, 32))}, {rng.Uint32(), uint32(max(0, prefix-32))}, {rng.Uint32(), uint32(count)}}
			binary.Write(&inputs, binary.LittleEndian, [4]uint32{0, 0, uint32(len(pairs)), 0})
			binary.Write(&inputs, binary.LittleEndian, pairs)
			w := rfxBitWriter{}
			for _, pair := range pairs {
				w.bits(pair[0], int(pair[1]))
			}
			expected = append(expected, w.data)
		}
	}
	modes := []struct {
		name   string
		number uint32
	}{{"native-scalar", 0}, {"native-sse2", 1}, {"native-avx2", 2}}
	if runtime.GOARCH == "arm64" {
		modes = modes[:1]
		modes = append(modes, struct {
			name   string
			number uint32
		}{"native", 1})
	}
	appendCoefficients := func(kind, mode, extra uint32, values []int32, want []byte) {
		binary.Write(&inputs, binary.LittleEndian, [4]uint32{kind, mode, uint32(len(values)), extra})
		binary.Write(&inputs, binary.LittleEndian, values)
		expected = append(expected, want)
	}
	for _, mode := range modes {
		codec, err := newNativeProgressive(mode.name, 6)
		if err != nil {
			continue
		}
		codec.close()
		for kind := 0; kind < 38; kind++ {
			values := make([]int32, 4096)
			switch kind {
			case 0: // Entirely zero, including the final run.
			case 1: // Dense coefficients exercise long unary prefixes.
				for i := range values {
					values[i] = int32(rng.Intn(65535) - 32767)
				}
			case 2:
				for i := 0; i < len(values); i += 2 {
					values[i] = -1
				}
			case 3:
				values[4095] = 1
			default: // Short runs exercise every SIMD lane and unaligned starts.
				for i := 0; i < len(values); i += kind - 3 {
					values[i] = int32(i%63) - 31
				}
			}
			appendCoefficients(1, mode.number, 0, values, encodeRLGR1(values))
		}
		for extra := uint32(1); extra <= 4; extra++ {
			for _, count := range []int{0, 1, 3, 4, 5, 7, 8, 9, 15, 16, 17, 31, 32, 33, 63, 64, 65, 127, 4015, 4096} {
				for kind := 0; kind < 2; kind++ {
					values := make([]int32, count)
					if kind == 1 {
						maximum := int32(1<<extra) - 1
						for i := range values {
							if rng.Intn(4) == 0 {
								values[i] = int32(rng.Intn(int(2*maximum+1))) - maximum
							}
						}
					}
					appendCoefficients(2, mode.number, extra, values, encodeProgressiveSRL(values, int(extra)))
				}
			}
		}
	}
	command = exec.Command(program)
	command.Stdin = &inputs
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("entropy fixture: %v\n%s", err, stderr.Bytes())
	}
	result := bytes.NewReader(output)
	for i, want := range expected {
		var size uint32
		if err := binary.Read(result, binary.LittleEndian, &size); err != nil {
			t.Fatal(err)
		}
		if size > 256<<10 {
			t.Fatal("invalid entropy size")
		}
		got := make([]byte, size)
		if _, err := io.ReadFull(result, got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("entropy case %d: native bytes differ from Go reference", i)
		}
	}
	if result.Len() != 0 {
		t.Fatal("unexpected entropy output")
	}
}
