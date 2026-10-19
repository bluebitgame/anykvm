//go:build linux && cgo

package gpuconv

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// Validate the generated binary's actual host/shader ABI, including bindings,
// workgroup size and cbuffer layout; stale or incompatible shaders fail offline.
func TestDesktopShaderABI(t *testing.T) {
	var manifest struct {
		Source string `json:"source_sha256"`
		SPIRV  string `json:"spirv_sha256"`
	}
	data, err := os.ReadFile("shaders/desktop.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("shaders/desktop.hlsl")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		data     []byte
		expected string
	}{{source, manifest.Source}, {shader, manifest.SPIRV}} {
		hash := sha256.Sum256(item.data)
		if hex.EncodeToString(hash[:]) != item.expected {
			t.Fatal("shader source/binary manifest is stale; run tools/build_shaders.py")
		}
	}
	if len(shader) < 20 || len(shader)%4 != 0 || binary.LittleEndian.Uint32(shader) != 0x07230203 {
		t.Fatal("invalid SPIR-V")
	}
	words := make([]uint32, len(shader)/4)
	for i := range words {
		words[i] = binary.LittleEndian.Uint32(shader[i*4:])
	}
	bindings := map[uint32]uint32{}
	sets := map[uint32]uint32{}
	members := map[uint32]map[uint32]uint32{}
	workgroup := false
	for offset := 5; offset < len(words); {
		count, op := int(words[offset]>>16), words[offset]&65535
		if count < 1 || offset+count > len(words) {
			t.Fatal("malformed SPIR-V instruction")
		}
		p := words[offset : offset+count]
		if op == 71 && count == 4 {
			if p[2] == 33 {
				bindings[p[1]] = p[3]
			}
			if p[2] == 34 {
				sets[p[1]] = p[3]
			}
		}
		if op == 72 && count == 5 && p[3] == 35 {
			if members[p[1]] == nil {
				members[p[1]] = map[uint32]uint32{}
			}
			members[p[1]][p[2]] = p[4]
		}
		if op == 16 && count == 6 && p[2] == 17 && p[3] == 8 && p[4] == 8 && p[5] == 1 {
			workgroup = true
		}
		offset += count
	}
	if !workgroup || len(bindings) != 4 {
		t.Fatalf("invalid shader workgroup or descriptor bindings: %v", bindings)
	}
	found := map[uint32]bool{}
	for id, binding := range bindings {
		if sets[id] != 0 || binding > 3 || found[binding] {
			t.Fatal("descriptor ABI mismatch")
		}
		found[binding] = true
	}
	layout := []uint32{0, 8, 16, 24, 32, 48, 52, 56, 60}
	matched := false
	for _, m := range members {
		if len(m) != len(layout) {
			continue
		}
		match := true
		for i, value := range layout {
			if m[uint32(i)] != value {
				match = false
			}
		}
		if match {
			matched = true
		}
	}
	if !matched {
		t.Fatal("HLSL constant-buffer offsets differ from native uint32[16]")
	}
}
