package videoenc

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestMediaFoundationRecoveryHeaders(t *testing.T) {
	sps, pps, idr := []byte{0x67, 0x42, 0x80}, []byte{0x68, 0xce}, []byte{0x65, 0x88, 0x84}
	header := []byte{1, 0x42, 0, 0x1f, 0xff, 0xe1, 0, byte(len(sps))}
	header = append(header, sps...)
	header = append(header, 1, 0, byte(len(pps)))
	header = append(header, pps...)
	data := binary.BigEndian.AppendUint32(nil, uint32(len(idr)))
	data = append(data, idr...)
	want := append(append(append([]byte{}, mfStartCode...), sps...), mfStartCode...)
	want = append(append(append(want, pps...), mfStartCode...), idr...)
	got, err := mediaFoundationPacket(data, header, "h264")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("got=%x want=%x error=%v", got, want, err)
	}
	// Length 256 resembles a three-byte Annex-B prefix; honor the AVC record.
	largeIDR := bytes.Repeat([]byte{0x88}, 256)
	largeIDR[0] = 0x65
	large := binary.BigEndian.AppendUint32(nil, uint32(len(largeIDR)))
	got, err = mediaFoundationPacket(append(large, largeIDR...), header, "h264")
	if err != nil || !bytes.HasSuffix(got, largeIDR) {
		t.Fatalf("ambiguous length prefix: %x error=%v", got, err)
	}
	// A driver that already repeats SPS/PPS must not acquire duplicates.
	got, err = mediaFoundationPacket(want, header, "h264")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("duplicated headers: %x error=%v", got, err)
	}
	// Predicted pictures must not receive the cached recovery headers.
	predicted := []byte{0, 0, 1, 0x41, 0x88}
	got, err = mediaFoundationPacket(predicted, header, "h264")
	if err != nil || !bytes.Equal(got, []byte{0, 0, 0, 1, 0x41, 0x88}) {
		t.Fatalf("predicted picture: %x error=%v", got, err)
	}
}

func TestMediaFoundationHEVCRecoveryHeaders(t *testing.T) {
	header := make([]byte, 23)
	header[0], header[21], header[22] = 1, 0xff, 3
	for _, tag := range []byte{32, 33, 34} {
		header = append(header, tag, 0, 1, 0, 3, tag<<1, 1, 0x80)
	}
	got, err := mediaFoundationPacket([]byte{0, 0, 0, 3, 38, 1, 0x80}, header, "hevc")
	if err != nil {
		t.Fatal(err)
	}
	units, err := mediaFoundationUnits(got, 4)
	if err != nil || len(units) != 4 {
		t.Fatalf("units=%x error=%v", units, err)
	}
	for i, tag := range []byte{32, 33, 34, 19} {
		if units[i][0]>>1 != tag {
			t.Fatalf("wrong HEVC recovery order: %x", units)
		}
	}
}

func TestMediaFoundationMalformedPackets(t *testing.T) {
	for _, data := range [][]byte{{}, {0, 0}, {0, 0, 0, 0}, {0, 0, 0, 8, 0x65}, {0, 0, 1}, {0, 0, 1, 0, 0, 1, 0x65}} {
		if _, err := mediaFoundationPacket(data, nil, "h264"); err == nil {
			t.Fatalf("accepted malformed packet %x", data)
		}
	}
	for _, header := range [][]byte{{1}, {1, 0, 0, 0, 0xff, 0xe1, 0, 9, 0x67}, make([]byte, 23)} {
		if _, err := mediaFoundationPacket([]byte{0, 0, 1, 0x65}, header, "h264"); err == nil {
			t.Fatalf("accepted malformed header %x", header)
		}
	}
}

func TestNativeWindowsAV1PacketFormat(t *testing.T) {
	// Vendor and MF output must be size-delimited OBUs, never an IVF container
	// or an undelimited AV1 frame. Predicted pictures do not require a sequence.
	for _, data := range [][]byte{nil, []byte("DKIF"), {0x30, 1, 0x10}, {0x32, 4, 0x10}} {
		if _, err := mediaFoundationPacket(data, nil, "av1"); err == nil {
			t.Fatalf("accepted invalid native AV1 output %x", data)
		}
	}
	predicted := []byte{0x32, 1, 0x10}
	if data, err := mediaFoundationPacket(predicted, nil, "av1"); err != nil || !bytes.Equal(data, predicted) {
		t.Fatalf("predicted native AV1 frame: %x error=%v", data, err)
	}
}
