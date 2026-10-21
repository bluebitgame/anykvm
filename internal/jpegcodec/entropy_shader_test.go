package jpegcodec

import (
	"bytes"
	"encoding/binary"
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
	source = source[:strings.Index(source, "#ifdef __spirv__")] + source[strings.Index(source, "// One restart interval"):]
	source = regexp.MustCompile(`\[numthreads\([^\]]+\)\]`).ReplaceAllString(source, "")
	source = strings.NewReplacer("inout EntropyWriter writer", "EntropyWriter &writer", "inout int previousDC", "int &previousDC",
		"inout uint mcu", "uint &mcu", "(EntropyWriter)0", "EntropyWriter{}", " : SV_DispatchThreadID", "").Replace(source)
	if err := os.WriteFile(filepath.Join(dir, "entropy_shader.hpp"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(dir, "entropy-shader")
	command := exec.Command(compiler, "-std=c++17", "-O2", "-I"+dir, "testdata/entropy_shader.cpp", "-o", program)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile entropy shader: %v\n%s", err, output)
	}
	var inputs bytes.Buffer
	var expected [][]byte
	for kind := 0; kind < 5; kind++ {
		w, h := 35, 19
		if kind == 4 {
			w, h = 256, 992 // One MCU per run/value combination in each AC table.
		}
		count := ((w + 15) / 16) * ((h + 15) / 16)
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
		}
		data, err := encodeQuantized420Restart(coefficients, w, h, 80, true)
		if err != nil {
			t.Fatal(err)
		}
		expected = append(expected, data)
		header := jpegHeader420(w, h, 80, true)
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
	}
	if result.Len() != 0 {
		t.Fatal("unexpected shader output")
	}
}
