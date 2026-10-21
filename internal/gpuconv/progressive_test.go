package gpuconv

import "testing"

func TestProgressiveMetadataBounds(t *testing.T) {
	valid := []uint32{1, 0, 6, 2, 1, 4, 0, 0, 3, 0, 0, 0, 0, 0, 0, 0}
	if n, err := ProgressiveMetadata(make([]byte, 16384), valid); err != nil || n != 4*12288*4 {
		t.Fatalf("valid metadata: bytes=%d err=%v", n, err)
	}
	for _, change := range []struct {
		word  int
		value uint32
	}{{0, 0}, {0, 9}, {1, 2}, {2, 5}, {2, 16}, {3, 0}, {3, 5}, {4, 3}, {5, 0}, {5, 2731}, {8, 4}, {8, 0xffffffff}} {
		m := append([]uint32(nil), valid...)
		m[change.word] = change.value
		if _, err := ProgressiveMetadata(make([]byte, 16384), m); err == nil {
			t.Fatalf("accepted invalid word %d = %d", change.word, change.value)
		}
	}
	if _, err := ProgressiveMetadata(make([]byte, 16383), valid); err == nil {
		t.Fatal("accepted short pixel input")
	}
	m := append([]uint32(nil), valid...)
	m[0] = 2
	m[9] = 3
	if _, err := ProgressiveMetadata(make([]byte, 32768), m); err == nil {
		t.Fatal("accepted aliased coefficient slots")
	}
	m = append([]uint32(nil), valid...)
	m[4] = 2
	if _, err := ProgressiveMetadata(make([]byte, 4), m); err != nil {
		t.Fatal(err)
	}
	m = append([]uint32(nil), valid...)
	m[1] = 1
	if _, err := ProgressiveMetadata(make([]byte, 6144), m); err != nil {
		t.Fatal(err)
	}
}

func TestProgressiveResidentMetadataBounds(t *testing.T) {
	// SIMPLE addresses the ninth screen tile while using only eight scratch
	// coefficient slots; FIRST uses the persistent nine-tile coefficient cache.
	m := []uint32{1, 0, 6, 0, 0, 8, 129 * 4, 131, 8, 0, 0, 0, 0, 0, 0, 0}
	if _, err := ProgressiveMetadata(make([]byte, 4), m); err != nil {
		t.Fatal(err)
	}
	m[3], m[4], m[5] = 2, 1, 9
	if _, err := ProgressiveMetadata(make([]byte, 4), m); err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct {
		index int
		value uint32
	}{{6, 3}, {6, 8193 * 4}, {7, 0}, {7, 8193}, {1, 1}, {5, 8}, {8, 9}, {4, 2}} {
		invalid := append([]uint32(nil), m...)
		invalid[change.index] = change.value
		if _, err := ProgressiveMetadata(make([]byte, 4), invalid); err == nil {
			t.Fatalf("accepted resident word %d=%d", change.index, change.value)
		}
	}
}
