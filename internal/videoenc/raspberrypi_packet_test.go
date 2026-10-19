package videoenc

import (
	"bytes"
	"testing"
)

func TestRaspberryPiRecoveryPackets(t *testing.T) {
	header := []byte{0, 0, 0, 1, 0x67, 0x42, 0, 0, 0, 1, 0x68, 0xce}
	idr := []byte{0, 0, 1, 0x65, 0x80}
	p := []byte{0, 0, 1, 0x41, 0x80}
	first := append(append([]byte(nil), header...), idr...)
	_, cached, err := raspberryPiPacket(first, nil, true)
	if err != nil || !bytes.Equal(cached, header) {
		t.Fatalf("initial recovery parameters: %x, %v", cached, err)
	}
	packet, next, err := raspberryPiPacket(idr, cached, true)
	if err != nil || !bytes.Equal(next, header) || !bytes.HasPrefix(packet, header) {
		t.Fatalf("cached recovery parameters: %x, %v", packet, err)
	}
	packet, next, err = raspberryPiPacket(p, cached, false)
	if err != nil || !bytes.Equal(next, header) || bytes.Contains(packet, header) {
		t.Fatalf("inter picture: %x, %v", packet, err)
	}
	for _, tc := range []struct {
		name         string
		data, header []byte
		force        bool
	}{
		{"missing parameters", idr, nil, true},
		{"ignored recovery request", p, cached, true},
		{"headers only", header, cached, false},
		{"corrupted packet", []byte{1, 2, 3}, cached, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := raspberryPiPacket(tc.data, tc.header, tc.force); err == nil {
				t.Fatal("accepted an unusable picture")
			}
		})
	}
}
