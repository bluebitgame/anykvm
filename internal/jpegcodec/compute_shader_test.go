package jpegcodec

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// Validate the generated Vulkan ABI on any build host, without a GPU.
func TestJPEGComputeShaderABI(t *testing.T) {
	for _, stage := range []struct {
		entry    string
		bindings []uint32
		group    [3]uint32
	}{
		{"compareTiles", []uint32{0, 2, 4, 6, 7, 8, 9}, [3]uint32{64, 1, 1}},
		{"prepareWork", []uint32{2, 7, 9}, [3]uint32{1, 1, 1}},
		{"main", []uint32{0, 1, 2, 9}, [3]uint32{8, 8, 1}},
		{"selectEntropy", []uint32{1, 2, 4, 6, 9}, [3]uint32{64, 1, 1}},
		{"prepareEntropy", []uint32{2, 7, 9}, [3]uint32{1, 1, 1}},
		{"encodeMCUs", []uint32{1, 2, 3, 4, 6, 9}, [3]uint32{64, 1, 1}},
		{"indexSegments", []uint32{2, 4, 9}, [3]uint32{256, 1, 1}},
		{"scanSegmentGroups", []uint32{2, 4, 7, 9}, [3]uint32{256, 1, 1}},
		{"addSegmentOffsets", []uint32{2, 4, 9}, [3]uint32{256, 1, 1}},
		{"packJPEG", []uint32{2, 3, 4, 5, 6, 9}, [3]uint32{64, 1, 1}},
	} {
		t.Run(stage.entry, func(t *testing.T) { checkJPEGShaderABI(t, stage.entry, stage.bindings, stage.group) })
	}
}

func checkJPEGShaderABI(t *testing.T, entry string, expectedBindings []uint32, group [3]uint32) {
	checkJPEGShaderProgramABI(t, "jpeg420", jpegComputeShader, entry, expectedBindings, group, true)
}

func checkJPEGShaderProgramABI(t *testing.T, sourceName, source, entry string, expectedBindings []uint32, group [3]uint32, constantBuffer bool) {
	t.Helper()
	suffix := ""
	if entry != "main" {
		suffix = "_" + entry
	}
	name := "shaders/" + sourceName + suffix
	spirv, err := os.ReadFile(name + ".spv")
	if os.IsNotExist(err) {
		t.Skip("run tools/build_shaders.py to validate JPEG SPIR-V")
	}
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Source string `json:"source_sha256"`
		SPIRV  string `json:"spirv_sha256"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		data     []byte
		expected string
	}{
		{[]byte(source), manifest.Source}, {spirv, manifest.SPIRV},
	} {
		hash := sha256.Sum256(item.data)
		if hex.EncodeToString(hash[:]) != item.expected {
			t.Fatal("JPEG shader manifest is stale; run tools/build_shaders.py")
		}
	}
	if len(spirv) < 20 || len(spirv)%4 != 0 || binary.LittleEndian.Uint32(spirv) != 0x07230203 {
		t.Fatal("invalid JPEG SPIR-V")
	}
	words := make([]uint32, len(spirv)/4)
	for i := range words {
		words[i] = binary.LittleEndian.Uint32(spirv[i*4:])
	}
	bindings, sets := map[uint32]uint32{}, map[uint32]uint32{}
	offsets := map[uint32]map[uint32]uint32{}
	workgroup := false
	for offset := 5; offset < len(words); {
		count, op := int(words[offset]>>16), words[offset]&65535
		if count < 1 || offset+count > len(words) {
			t.Fatal("malformed JPEG SPIR-V")
		}
		instruction := words[offset : offset+count]
		switch {
		case op == 71 && count == 4: // OpDecorate: Binding / DescriptorSet.
			if instruction[2] == 33 {
				bindings[instruction[1]] = instruction[3]
			}
			if instruction[2] == 34 {
				sets[instruction[1]] = instruction[3]
			}
		case op == 72 && count == 5 && instruction[3] == 35: // OpMemberDecorate: Offset.
			id := instruction[1]
			if offsets[id] == nil {
				offsets[id] = map[uint32]uint32{}
			}
			offsets[id][instruction[2]] = instruction[4]
		case op == 16 && count == 6 && instruction[2] == 17: // OpExecutionMode: LocalSize.
			workgroup = instruction[3] == group[0] && instruction[4] == group[1] && instruction[5] == group[2]
		}
		offset += count
	}
	if !workgroup || len(bindings) != len(expectedBindings) {
		t.Fatal("JPEG workgroup/binding count mismatch")
	}
	seen := map[uint32]bool{}
	for id, binding := range bindings {
		set, exists := sets[id]
		valid := false
		for _, expected := range expectedBindings {
			if binding == expected {
				valid = true
			}
		}
		if !exists || set != 0 || !valid || seen[binding] {
			t.Fatal("JPEG descriptor ABI mismatch")
		}
		seen[binding] = true
	}
	if !constantBuffer {
		return
	}
	want := []uint32{0, 8, 16, 20, 24, 28, 32, 40, 48, 560, 12848}
	for _, members := range offsets {
		if len(members) != len(want) {
			continue
		}
		matches := true
		for index, offset := range want {
			if members[uint32(index)] != offset {
				matches = false
			}
		}
		if matches {
			return
		}
	}
	t.Fatal("JPEG constant-buffer offsets differ from host layout")
}

func TestJPEGDecodeShaderABI(t *testing.T) {
	source, err := os.ReadFile("shaders/jpeg_decode.hlsl")
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []struct {
		entry    string
		bindings []uint32
		group    [3]uint32
	}{
		{"main", []uint32{0, 1, 2, 5}, [3]uint32{64, 1, 1}},
		{"inverseDCT", []uint32{1, 2, 3}, [3]uint32{8, 8, 1}},
		{"packNV12", []uint32{1, 3, 4}, [3]uint32{64, 1, 1}},
	} {
		t.Run(stage.entry, func(t *testing.T) {
			checkJPEGShaderProgramABI(t, "jpeg_decode", string(source), stage.entry, stage.bindings, stage.group, false)
		})
	}
}
