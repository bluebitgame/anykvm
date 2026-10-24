package jpegcodec

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestJPEGEntropyShaderBytes(t *testing.T) {
	compiler, err := exec.LookPath("c++")
	if err != nil {
		t.Skip("C++ compiler unavailable")
	}
	dir := t.TempDir()
	raw, err := os.ReadFile("shaders/jpeg420.hlsl")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	gate := source[strings.Index(source, "// MCU tile gate."):strings.Index(source, "// Shared DCT workspace.")]
	source = source[:strings.Index(source, "#ifdef __spirv__")] + gate + source[strings.Index(source, "// One restart interval"):]
	source = regexp.MustCompile(`\[numthreads\([^\]]+\)\]`).ReplaceAllString(source, "")
	source = strings.NewReplacer("ENTROPY_LOOP", "", "inout EntropyWriter writer", "EntropyWriter &writer", "inout int previousDC", "int &previousDC",
		"inout uint mcu", "uint &mcu", "(EntropyWriter)0", "EntropyWriter{}", "groupshared ", "",
		" : SV_DispatchThreadID", "", " : SV_GroupID", "", " : SV_GroupThreadID", "").Replace(source)
	// Count actual coefficient requests in the executed shader. DC-only blocks
	// must not load AC data, and sparse blocks must load only their nonzero ACs.
	source = strings.Replace(source, "uint packed = outputCoefficients[blockOffset + index / 2];",
		"coefficientLoads++;\n    uint packed = outputCoefficients[blockOffset + index / 2];", 1)
	source = strings.Replace(source, "void writeSymbol(EntropyWriter &writer, uint table, uint symbol) {", "void writeSymbol(EntropyWriter &writer, uint table, uint symbol) { huffmanWrites++;", 1)
	source = strings.Replace(source, "uint count) {\n    uint index = table * 256 + symbol;", "uint count) {\n    huffmanWrites++;\n    uint index = table * 256 + symbol;", 1)
	if err := os.WriteFile(filepath.Join(dir, "entropy_shader.hpp"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(dir, "entropy-shader")
	command := exec.Command(compiler, "-std=c++17", "-O2", "-pthread", "-I"+dir, "testdata/entropy_shader.cpp", "-o", program)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile entropy shader: %v\n%s", err, output)
	}
	if output, err := exec.Command(program, "writer").CombinedOutput(); err != nil {
		t.Fatalf("bit writer alignment/stuffing: %v\n%s", err, output)
	}
	var inputs bytes.Buffer
	var expected [][]byte
	for kind := 0; kind < 14; kind++ {
		w, h := 35, 19
		if kind == 4 {
			w, h = 256, 992 // One MCU per run/value combination in each AC table.
		}
		if kind == 5 {
			w, h = 63*16, 16 // Isolate every AC position, including mask bits 31/32/63.
		}
		count := ((w + 15) / 16) * ((h + 15) / 16)
		quality := 80
		if kind >= 7 && kind <= 10 {
			quality = []int{1, 19, 50, 100}[kind-7]
		}
		if kind >= 12 {
			quality = 100
		}
		coefficients := make([]int32, count*384)
		rng := rand.New(rand.NewSource(91))
		for base := 0; base < len(coefficients); base += 64 {
			coefficients[base] = int32(rng.Intn(2048) - 1024)
			for i := 1; i < 64; i++ {
				switch kind {
				case 0: // Dense small AC coefficients.
					coefficients[base+i] = int32(rng.Intn(63) - 31)
				case 1: // Long canonical codes, maximum amplitudes and stuffing.
					coefficients[base+i] = int32(rng.Intn(2047) - 1023)
				case 2: // ZRL, an AC at the last scan position and signed extremes.
					if i == 63 {
						coefficients[base+i] = -1023
					}
				}
			}
			if kind == 4 {
				mcu := base / 384
				run, value := mcu%16, mcu/16-31
				if value >= 0 {
					value++
				}
				coefficients[base+int(jpegZigzag[run+1])] = int32(value)
			}
			if kind == 5 {
				scan := base/384 + 1
				value := int32(31)
				if scan%2 == 0 {
					value = -1023
				}
				coefficients[base+int(jpegZigzag[scan])] = value
			}
			if kind >= 6 {
				block := base / 64 % 6
				component := 0
				if block >= 4 {
					component = block - 3
				}
				q := quantization420(quality)
				if kind == 6 {
					coefficients[base] = 0
					if component == 0 {
						coefficients[base] = -171 // q80 => exact black; not "no color".
					}
				} else {
					table := 0
					if component > 0 {
						table = 64
					}
					coefficients[base] = [3]int32{-37, 12, -18}[component] * 8 / int32(q[table])
				}
				if kind == 11 && (base/384)%2 != 0 {
					coefficients[base+1] = int32(block + 1) // Mixed descriptor/entropy records.
				}
				if kind == 12 {
					coefficients[base] = [3]int32{-1024, 1023, -1024}[component]
				}
				if kind == 13 {
					coefficients[base] = [3]int32{1023, -1024, 1023}[component]
				}
			}
		}
		data, err := encodeQuantized420Restart(coefficients, w, h, quality, true)
		if err != nil {
			t.Fatal(err)
		}
		expected = append(expected, data)
		header := jpegHeader420(w, h, quality, true)
		binary.Write(&inputs, binary.LittleEndian, uint32(count))
		binary.Write(&inputs, binary.LittleEndian, uint32(len(header)))
		binary.Write(&inputs, binary.LittleEndian, jpegPackedEntropyCodes)
		inputs.Write(header)
		binary.Write(&inputs, binary.LittleEndian, coefficients)
	}
	command = exec.Command(program)
	command.Stdin = &inputs
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("entropy shader: %v\n%s", err, stderr.Bytes())
	}
	result := bytes.NewReader(output)
	for n, want := range expected {
		var size uint32
		if err := binary.Read(result, binary.LittleEndian, &size); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, size)
		if _, err := io.ReadFull(result, got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("case %d: GPU entropy differs from scalar JPEG oracle", n)
		}
		if _, err := jpeg.Decode(bytes.NewReader(got)); err != nil {
			t.Fatal(err)
		}
		reference, err := ParseRestartJPEG(want)
		if err != nil {
			t.Fatal(err)
		}
		checker := reference.NewSolidChecker()
		var snapshot *RestartJPEG
		for pass := 0; pass < 4; pass++ {
			if err := binary.Read(result, binary.LittleEndian, &size); err != nil {
				t.Fatal(err)
			}
			packet := make([]byte, size)
			if _, err := io.ReadFull(result, packet); err != nil {
				t.Fatal(err)
			}
			snapshot, err = updateGPURegions(snapshot, reference.header, packet, reference.Width, reference.Height)
			if err != nil {
				t.Fatalf("case %d pass %d: %v", n, pass, err)
			}
			reconstructed, err := snapshot.Crop(image.Rect(0, 0, reference.Width, reference.Height))
			if err != nil || !bytes.Equal(reconstructed, want) {
				t.Fatalf("compact packet differs from oracle: case %d pass %d: %v", n, pass, err)
			}
			if !snapshot.HasRecordedColors() {
				t.Fatal("GPU packet did not retain encoder color metadata")
			}
			for i := range reference.segments {
				x, y := i%((reference.Width+15)/16)*16, i/((reference.Width+15)/16)*16
				r := image.Rect(x, y, min(x+16, reference.Width), min(y+16, reference.Height))
				wantColor, wantSolid := checker.Color(r)
				color, solid, known := snapshot.RecordedSolidColor(r)
				if !known || solid != wantSolid || (solid && color != wantColor) {
					t.Fatalf("case %d pass %d MCU %d metadata=(%06x,%v,%v) want=(%06x,%v)", n, pass, i, color, solid, known, wantColor, wantSolid)
				}
			}
			records := 0
			for pos := 4; pos < len(packet); {
				index := int(binary.LittleEndian.Uint32(packet[pos:]))
				tagged := binary.LittleEndian.Uint32(packet[pos+4:])
				length := int(tagged)
				if tagged&gpuSolidMCUTag != 0 {
					length = 0
					if len(snapshot.segments[index]) != 0 {
						t.Fatal("pure MCU retained entropy bytes")
					}
				}
				if pass == 1 || (pass == 2 && index/4 != (len(reference.segments)-1)/4) || (pass == 3 && (index/4)%2 != 0) {
					t.Fatal("clean MCU read back")
				}
				records++
				pos += 12 + (length+3)&^3
			}
			if ((n >= 6 && n <= 10) || n >= 12) && len(packet) != 4+records*12 {
				t.Fatal("all-solid frame contained entropy payload bytes")
			}

			if pass == 3 {
				expected := 0
				for index := range reference.segments {
					if (index/4)%2 == 0 {
						expected++
					}
				}
				if records != expected {
					t.Fatalf("sparse readback records=%d want=%d", records, expected)
				}
			}
			if (pass == 0 && records != len(reference.segments)) || (pass == 1 && (records != 0 || len(packet) != 4)) || (pass == 2 && records != (len(reference.segments)-1)%4+1) {
				t.Fatalf("unexpected readback record count %d", records)
			}
		}

	}
	if result.Len() != 0 {
		t.Fatal("unexpected shader output")
	}
}
