package jpegcodec

import (
	"bytes"
	"encoding/binary"
	"image"
	"os"
	"testing"
)

func gpuRegionPacket(j *RestartJPEG, selected func(int) bool) []byte {
	out := binary.LittleEndian.AppendUint32(nil, uint32(len(j.segments)))
	checker := j.NewSolidChecker()
	for i, s := range j.segments {
		if !selected(i) {
			continue
		}
		data := bytes.Clone(s)
		if i+1 < len(j.segments) {
			data = append(data, 0xff, byte(0xd0+i%8))
		}
		out = binary.LittleEndian.AppendUint32(out, uint32(i))
		out = binary.LittleEndian.AppendUint32(out, uint32(len(data)))
		x, y := (i%((j.Width+15)/16))*16, (i/((j.Width+15)/16))*16
		_, solid := checker.Color(image.Rect(x, y, min(x+16, j.Width), min(y+16, j.Height)))
		if solid {
			// Test oracle reads the DCs independently from the source entropy.
			reader := solidBits{data: s, stuffed: true}
			meta := checker.meta
			var predictors, dc [3]int
			if !solidMCU(meta, &reader, &predictors, &dc, true) {
				panic("invalid solid fixture")
			}
			tag := gpuSolidMCUTag | (uint32(dc[2])&0x400)<<20
			packed := uint32(dc[0])&0x7ff | (uint32(dc[1])&0x7ff)<<11 | (uint32(dc[2])&0x3ff)<<22
			// Replace the length word just appended; no entropy bytes follow.
			binary.LittleEndian.PutUint32(out[len(out)-4:], tag)
			out = binary.LittleEndian.AppendUint32(out, packed)
			continue
		}
		out = binary.LittleEndian.AppendUint32(out, 0)
		out = append(out, data...)
		for len(out)%4 != 0 {
			out = append(out, 0)
		}
	}
	return out
}

func TestGPUJPEGRegionSnapshots(t *testing.T) {
	var snapshot *RestartJPEG
	var first *RestartJPEG
	for _, name := range []string{"base", "changed", "later"} {
		data := restartWireData(t, name)
		reference, err := ParseRestartJPEG(data)
		if err != nil {
			t.Fatal(err)
		}
		previous := snapshot
		packet := gpuRegionPacket(reference, func(i int) bool { return previous == nil || !bytes.Equal(previous.segments[i], reference.segments[i]) })
		snapshot, err = updateGPURegions(previous, reference.header, packet, reference.Width, reference.Height)
		if err != nil {
			t.Fatal(err)
		}
		full, err := snapshot.Crop(image.Rect(0, 0, reference.Width, reference.Height))
		if err != nil || !bytes.Equal(full, data) {
			t.Fatalf("snapshot %s differs: %v", name, err)
		}
		if first == nil {
			first = snapshot
		}
		// Empty updates retain all immutable segments, without needing full readback.
		clean := binary.LittleEndian.AppendUint32(nil, uint32(len(reference.segments)))
		unchanged, err := updateGPURegions(snapshot, reference.header, clean, reference.Width, reference.Height)
		if err != nil || !snapshot.EqualRect(unchanged, image.Rect(0, 0, reference.Width, reference.Height)) {
			t.Fatal("empty update lost baseline", err)
		}
	}
	original, _ := first.Crop(image.Rect(0, 0, first.Width, first.Height))
	if !bytes.Equal(original, restartWireData(t, "base")) {
		t.Fatal("retained frame mutated")
	}
}

func TestGPUJPEGRegionPacketValidation(t *testing.T) {
	source, _ := ParseRestartJPEG(restartWireData(t, "base"))
	complete := gpuRegionPacket(source, func(int) bool { return true })
	for _, packet := range [][]byte{nil, complete[:3], complete[:len(complete)-1], binary.LittleEndian.AppendUint32(nil, uint32(len(source.segments))), append(bytes.Clone(complete), complete[4:16]...)} {
		if _, err := updateGPURegions(nil, source.header, packet, source.Width, source.Height); err == nil {
			t.Fatal("accepted incomplete or invalid packet")
		}
	}
	changed := bytes.Clone(complete)
	binary.LittleEndian.PutUint32(changed[8:], 3073)
	if _, err := updateGPURegions(nil, source.header, changed, source.Width, source.Height); err == nil {
		t.Fatal("accepted segment overflow")
	}
	quality := jpegHeader420(source.Width, source.Height, 81, true)
	empty := binary.LittleEndian.AppendUint32(nil, uint32(len(source.segments)))
	if _, err := updateGPURegions(source, quality, empty, source.Width, source.Height); err == nil {
		t.Fatal("reused old quantizer segments")
	}
	for _, invalid := range []uint32{1, 0x80000000, gpuSolidFlag | 0x02000000} {
		badColor := bytes.Clone(complete)
		binary.LittleEndian.PutUint32(badColor[12:], invalid)
		if _, err := updateGPURegions(nil, source.header, badColor, source.Width, source.Height); err == nil {
			t.Fatalf("accepted invalid GPU color word %08x", invalid)
		}
	}
}

func TestGPURecordedColorsAreImmutableAndNeedNoEntropy(t *testing.T) {
	const w, h = 65, 67
	coeff := constantJPEGCoefficients(w, h, [3]int32{})
	makeSource := func() *RestartJPEG {
		t.Helper()
		data, err := encodeQuantized420Restart(coeff, w, h, 80, true)
		if err != nil {
			t.Fatal(err)
		}
		j, err := ParseRestartJPEG(data)
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	first := makeSource()
	if _, _, known := first.RecordedSolidColor(image.Rect(0, 0, w, h)); known {
		t.Fatal("external JPEG incorrectly claims GPU metadata")
	}
	initial, err := updateGPURegions(nil, first.header, gpuRegionPacket(first, func(int) bool { return true }), w, h)
	if err != nil {
		t.Fatal(err)
	}
	assertColor := func(j *RestartJPEG, r image.Rectangle, want uint32, wantSolid bool) {
		t.Helper()
		color, solid, known := j.RecordedSolidColor(r)
		if !known || solid != wantSolid || (solid && color != want) {
			t.Fatalf("color metadata at %v = (%06x,%v,%v), want (%06x,%v)", r, color, solid, known, want, wantSolid)
		}
	}
	assertColor(initial, image.Rect(0, 0, w, h), 0x808080, true)
	// Drop all entropy storage in a private test view: recorded color queries
	// must still work, while the coefficient probe can no longer read the JPEG.
	probe := *initial
	probe.segments = make([][]byte, len(initial.segments))
	assertColor(&probe, image.Rect(0, 0, w, h), 0x808080, true)
	compat, err := probe.Crop(image.Rect(0, 0, w, h))
	if err != nil || !bytes.Equal(compat, mustCropGPUFixture(t, first)) {
		t.Fatal("constant JPEG compatibility crop lost original coefficients", err)
	}
	// A sparse new capture updates one MCU's color; all other colors inherit.
	for block := 0; block < 4; block++ {
		coeff[block*64] = 1
	}
	second := makeSource()
	next, err := updateGPURegions(initial, second.header, gpuRegionPacket(second, func(i int) bool { return i == 0 }), w, h)
	if err != nil {
		t.Fatal(err)
	}
	assertColor(initial, image.Rect(0, 0, w, h), 0x808080, true)
	assertColor(next, image.Rect(0, 0, 16, 16), 0x818181, true)
	assertColor(next, image.Rect(16, 0, 32, 16), 0x808080, true)
	assertColor(next, image.Rect(0, 0, w, h), 0, false)
	// Then skip a capture before replacing another solid MCU with texture.
	coeff[384+1] = 2
	third := makeSource()
	latest, err := updateGPURegions(next, third.header, gpuRegionPacket(third, func(i int) bool { return i == 1 }), w, h)
	if err != nil {
		t.Fatal(err)
	}
	assertColor(latest, image.Rect(0, 0, 16, 16), 0x818181, true)
	assertColor(latest, image.Rect(16, 0, 32, 16), 0, false)
	assertColor(next, image.Rect(16, 0, 32, 16), 0x808080, true)
	assertColor(latest, image.Rect(64, 64, w, h), 0x808080, true)
	empty := binary.LittleEndian.AppendUint32(nil, uint32(len(latest.segments)))
	static, err := updateGPURegions(latest, third.header, empty, w, h)
	if err != nil || static != latest {
		t.Fatal("empty update did not preserve the immutable snapshot and colors", err)
	}
	// Returning from texture to a constant must drop the old entropy slice.
	coeff[384+1] = 0
	fourth := makeSource()
	restored, err := updateGPURegions(latest, fourth.header, gpuRegionPacket(fourth, func(i int) bool { return i == 1 }), w, h)
	if err != nil || len(restored.segments[1]) != 0 || len(latest.segments[1]) == 0 {
		t.Fatal("solid update retained texture entropy or mutated the old snapshot", err)
	}
	assertColor(restored, image.Rect(16, 0, 32, 16), 0x808080, true)
	if initial.EqualRect(next, image.Rect(0, 0, 16, 16)) || initial.EqualMovedRect(next, image.Rect(0, 0, 16, 16), image.Rect(16, 0, 32, 16)) {
		t.Fatal("different metadata-only colors compared equal")
	}
	if !next.EqualMovedRect(restored, image.Rect(0, 0, 16, 16), image.Rect(0, 0, 16, 16)) {
		t.Fatal("equal metadata-only colors failed move comparison")
	}
}

func mustCropGPUFixture(t *testing.T, source *RestartJPEG) []byte {
	t.Helper()
	data, err := source.Crop(image.Rect(0, 0, source.Width, source.Height))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func restartWireData(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/restart_" + name + ".jpeg")
	if err != nil {
		t.Fatal(err)
	}
	return data
}
