package av1

import (
	"encoding/binary"
	"os"
	"testing"
)

func TestInspectEncodedAV1(t *testing.T) {
	data, err := os.ReadFile("../testdata/stream.ivf")
	if err != nil {
		t.Fatal(err)
	}
	data = data[32:]
	for frame := 0; frame < 6; frame++ {
		size := int(binary.LittleEndian.Uint32(data))
		au := data[12 : 12+size]
		data = data[12+size:]
		info, err := Inspect(au)
		if err != nil || info.Recovery != (frame%3 == 0) {
			t.Fatalf("frame %d: %+v %v", frame, info, err)
		}
		if info.Recovery && info.Codec != "av01.0.00M.08" {
			t.Fatalf("codec %q", info.Codec)
		}
	}
	if len(data) != 0 {
		t.Fatal("unexpected trailing frames")
	}
}
func TestInspectRejectsMalformedOBUs(t *testing.T) {
	for _, data := range [][]byte{nil, {0x80}, {0x30, 1, 0x10}, {0x32, 4, 0x10}, {0x32, 0x80}, {0x32, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, {0x0a, 1, 0}, {0x0a, 1, 0xe0}, {0x36, 7, 1, 0x10}} {
		if _, err := Inspect(data); err == nil {
			t.Fatalf("accepted %x", data)
		}
	}
	// Frame without a sequence header, show-existing-frame, and sequence-only
	// data must never be accepted as a recovery point.
	for _, data := range [][]byte{{0x32, 1, 0x10}, {0x32, 1, 0x80}, {0x12, 0}, {0x0a, 2, 0x18, 0}} {
		info, err := Inspect(data)
		if err != nil || info.Recovery {
			t.Fatalf("recovery %x: %+v %v", data, info, err)
		}
	}
}
func FuzzInspect(f *testing.F) {
	f.Add([]byte{0x12, 0, 0x32, 1, 0x10})
	f.Fuzz(func(t *testing.T, data []byte) { Inspect(data) })
}
