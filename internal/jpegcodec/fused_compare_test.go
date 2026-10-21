package jpegcodec

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Run actual comparison hooks and the actual DCT together, with the original
// full DCT as an exact oracle. The fixture counts texture loads, poisons omitted
// coefficient storage, and verifies static/alpha-only/texture transitions.
func TestJPEGFusedCompareSkipsDCT(t *testing.T) {
	compiler, err := exec.LookPath("c++")
	if err != nil {
		t.Skip("C++ compiler unavailable")
	}
	raw, err := os.ReadFile("shaders/jpeg420.hlsl")
	if err != nil {
		t.Fatal(err)
	}
	shader := string(raw)
	constants := shader[:strings.Index(shader, "#ifdef __spirv__")]
	gate := shader[strings.Index(shader, "// MCU tile gate."):strings.Index(shader, "// Shared DCT workspace.")]
	dct := shader[strings.Index(shader, "// Shared DCT workspace."):strings.Index(shader, "// One restart interval")]
	begin, end := strings.Index(dct, "uint2 sourcePosition("), strings.Index(dct, "float lumaSample(")
	dct = dct[:begin] + dct[end:]
	hooks := shader[strings.Index(shader, "// JPEG solid comparison hooks."):strings.Index(shader, "// ANYKVM_TILE_COMPARE")]
	load := shader[strings.Index(shader, "int loadCoefficient("):strings.Index(shader, "bool encodeBlock(")]
	store := shader[strings.Index(shader, "uint jpegSolidOffset("):strings.Index(shader, "[numthreads(64, 1, 1)]\nvoid encodeMCUs")]
	common, err := os.ReadFile("../gpuconv/shaders/tile_compare.hlsl")
	if err != nil {
		t.Fatal(err)
	}
	appendWork := shader[strings.Index(shader, "void compareFlag("):strings.Index(shader, "// JPEG solid comparison hooks.")]
	prepare := shader[strings.Index(shader, "// Comparison appends work"):strings.Index(shader, "float lumaSample(")]
	source := constants + gate + appendWork + hooks + dct + load + store + prepare + string(common)
	source = regexp.MustCompile(`\[numthreads\([^\]]+\)\]`).ReplaceAllString(source, "")
	source = strings.NewReplacer("groupshared ", "", "[unroll]", "", "void main(", "void transformMain(",
		"inout JPEGCompareState state", "JPEGCompareState &state",
		"inout uint lowMask", "uint &lowMask", "inout uint highMask", "uint &highMask",
		" : SV_DispatchThreadID", "", " : SV_GroupID", "", " : SV_GroupThreadID", "", " : SV_GroupIndex", "",
		"groupID.xy", "uint2(groupID.x,groupID.y)", "threadID.xy", "uint2(threadID.x,threadID.y)").Replace(source)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "encode_shader.hpp"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(dir, "fused-compare")
	command := exec.Command(compiler, "-std=c++17", "-DFUSED_COMPARE", "-O2", "-pthread", "-I"+dir, "testdata/encode_shader.cpp", "-o", program)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile fused shader fixture: %v\n%s", err, output)
	}
	fixtures := []struct {
		name string
		w, h int
		p    []byte
	}{
		{"blue-edge", 1, 1, []byte{255, 0, 0, 255}},
		{"solid-alpha", 35, 19, bytes.Repeat([]byte{241, 87, 123, 255}, 35*19)},
		{"MCU-mosaic", 65, 17, make([]byte, 65*17*4)},
		{"near-flat", 35, 19, make([]byte, 35*19*4)},
		{"noise-clipped", 65, 17, make([]byte, 65*17*4)},
		{"black", 64, 64, make([]byte, 64*64*4)},
		{"vertical-edge", 17, 65, bytes.Repeat([]byte{39, 173, 79, 255}, 17*65)},
	}
	for i := 3; i < len(fixtures[1].p); i += 4 {
		fixtures[1].p[i] = byte(i)
	}
	for y := 0; y < 17; y++ {
		for x := 0; x < 65; x++ {
			i := (y*65 + x) * 4
			copy(fixtures[2].p[i:i+4], []byte{byte(x / 16 * 43), byte(y / 16 * 91), byte(191 - x/16*29), byte(x + y)})
		}
	}
	for i := 0; i < len(fixtures[3].p); i += 4 {
		v := byte(127 + i/4%2)
		copy(fixtures[3].p[i:i+4], []byte{v, v, v, 255})
	}
	rand.New(rand.NewSource(61)).Read(fixtures[4].p)
	for _, fixture := range fixtures {
		for _, quality := range []int{1, 19, 80, 100} {
			var input bytes.Buffer
			binary.Write(&input, binary.LittleEndian, uint32(fixture.w))
			binary.Write(&input, binary.LittleEndian, uint32(fixture.h))
			binary.Write(&input, binary.LittleEndian, quantization420(quality))
			input.Write(fixture.p)
			command := exec.Command(program)
			command.Stdin = &input
			output, err := command.CombinedOutput()
			if err != nil || len(output) != 12 {
				t.Fatalf("%s q%d fused pipeline: %v\n%s", fixture.name, quality, err, output)
			}
			pure := 0
			for y := 0; y < fixture.h; y += 16 {
				for x := 0; x < fixture.w; x += 16 {
					first, same := (y*fixture.w+x)*4, true
					for yy := y; yy < min(y+16, fixture.h); yy++ {
						for xx := x; xx < min(x+16, fixture.w); xx++ {
							i := (yy*fixture.w + xx) * 4
							same = same && bytes.Equal(fixture.p[first:first+3], fixture.p[i:i+3])
						}
					}
					if same {
						pure++
					}
				}
			}
			count := ((fixture.w + 15) / 16) * ((fixture.h + 15) / 16)
			if int(binary.LittleEndian.Uint32(output)) != pure ||
				int(binary.LittleEndian.Uint32(output[4:])) != fixture.w*fixture.h ||
				int(binary.LittleEndian.Uint32(output[8:])) != (count-pure)*512 {
				t.Fatalf("%s q%d incorrect solid count or repeated texture loads: %v", fixture.name, quality, output)
			}
		}
	}
}
