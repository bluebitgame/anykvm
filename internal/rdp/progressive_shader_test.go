package rdp

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"testing"
)

// Check generated shader provenance and descriptor/workgroup ABI on all hosts.
func TestProgressiveShaderABI(t *testing.T) {
	for _, stage := range []struct {
		source   string
		entry    string
		bindings []uint32
		group    [3]uint32
	}{
		{"progressive", "main", []uint32{0, 1, 2}, [3]uint32{64, 1, 1}},
		{"progressive", "encodeTiles", []uint32{1, 2, 3, 4}, [3]uint32{1, 1, 1}},
		{"progressive", "indexSegments", []uint32{1, 4}, [3]uint32{1, 1, 1}},
		{"progressive", "packSegments", []uint32{3, 4, 5}, [3]uint32{64, 1, 1}},
		{"progressive_surface", "main", []uint32{0, 1, 2, 3, 4}, [3]uint32{64, 1, 1}},
	} {
		t.Run(stage.source+"/"+stage.entry, func(t *testing.T) {
			name := "shaders/" + stage.source
			if stage.entry != "main" {
				name += "_" + stage.entry
			}
			spv, err := os.ReadFile(name + ".spv")
			if os.IsNotExist(err) {
				t.Skip("run tools/build_shaders.py for generated SPIR-V")
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(name + ".json")
			if err != nil {
				t.Fatal(err)
			}
			var manifest struct {
				Entry  string `json:"entry_point"`
				Source string `json:"source_sha256"`
				SPIRV  string `json:"spirv_sha256"`
			}
			if err = json.Unmarshal(data, &manifest); err != nil {
				t.Fatal(err)
			}
			source := progressiveShader
			if stage.source == "progressive_surface" {
				source = progressiveSurfaceShader
			}
			sourceHash, spvHash := sha256.Sum256([]byte(source)), sha256.Sum256(spv)
			if manifest.Entry != stage.entry || manifest.Source != hex.EncodeToString(sourceHash[:]) || manifest.SPIRV != hex.EncodeToString(spvHash[:]) {
				t.Fatal("stale Progressive shader assets")
			}
			if len(spv) < 20 || len(spv)%4 != 0 || binary.LittleEndian.Uint32(spv) != 0x07230203 {
				t.Fatal("invalid SPIR-V")
			}
			words := make([]uint32, len(spv)/4)
			for i := range words {
				words[i] = binary.LittleEndian.Uint32(spv[i*4:])
			}
			bindings := []uint32{}
			group := false
			for i := 5; i < len(words); {
				count, op := int(words[i]>>16), words[i]&65535
				if count < 1 || i+count > len(words) {
					t.Fatal("malformed SPIR-V")
				}
				p := words[i : i+count]
				if op == 71 && count == 4 && p[2] == 33 {
					bindings = append(bindings, p[3])
				}
				if op == 71 && count == 4 && p[2] == 34 && p[3] != 0 {
					t.Fatal("unexpected descriptor set")
				}
				if op == 16 && count == 6 && p[2] == 17 {
					group = [3]uint32{p[3], p[4], p[5]} == stage.group
				}
				i += count
			}
			sort.Slice(bindings, func(i, j int) bool { return bindings[i] < bindings[j] })
			if !group || !reflect.DeepEqual(bindings, stage.bindings) {
				t.Fatalf("ABI mismatch: bindings=%v workgroup=%v", bindings, group)
			}
		})
	}
}
