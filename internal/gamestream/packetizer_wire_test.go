//go:build cgo

package gamestream

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"testing"
)

// These hashes pin the pre-optimization wire bytes, including FEC, padding,
// frame/block headers, IVs and GCM tags across short and multi-block frames.
func TestVideoPacketWireGolden(t *testing.T) {
	for _, tc := range []struct {
		n, size int
		want    string
	}{
		{7, 512, "8a697491b74acbae9fd0e3f2571a31742fd70e234cf9ed547d193a2bad8572ec"},
		{100000, 1024, "a08ce7fa890b2532f2615efa4cf55a5c5e947143a40f591d7110dfdd25d97503"},
		{400000, 512, "55390c43b1825466d381861ec0371904f094d2712fe9dc70af788dea82aebbe0"},
		{700000, 1408, "a439cf5080a96fae9a53a60bfb336298c6c55ddce5bacd3af5704f964a07fd62"},
	} {
		v := &videoPacketizer{sequence: 65530, frame: 31, iv: 1234}
		block, _ := aes.NewCipher([]byte("0123456789abcdef"))
		v.gcm, _ = cipher.NewGCM(block)
		au := make([]byte, tc.n)
		for i := range au {
			au[i] = byte(i*37 + 11)
		}
		batch, err := v.packBatch(au, true, 98765, tc.size, 2)
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.New()
		for _, packet := range batch.packets {
			var size [4]byte
			binary.BigEndian.PutUint32(size[:], uint32(len(packet)))
			h.Write(size[:])
			h.Write(packet)
		}
		batch.Release()
		got := fmt.Sprintf("%x", h.Sum(nil))
		if got != tc.want {
			t.Fatalf("n=%d size=%d wire changed: %s", tc.n, tc.size, got)
		}
	}
}
