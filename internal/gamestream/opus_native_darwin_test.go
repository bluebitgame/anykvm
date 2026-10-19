//go:build darwin && cgo

package gamestream

import (
	"os"
	"testing"
)

// Opt-in synthetic software audio check, no microphone or capture permissions.
func TestSystemOpusPackets(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_SYSTEM_OPUS") == "" {
		t.Skip("set ANYKVM_TEST_SYSTEM_OPUS for AudioToolbox software encode")
	}
	for _, duration := range []int{5, 10} {
		e, err := newNativeOpusEncoder(duration)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for i := 0; i < 20; i++ {
			packet, err := e.Encode(make([]byte, duration*48*4))
			if err != nil {
				e.Close()
				t.Fatal(err)
			}
			if len(packet) > 0 {
				count++
				// CELT-only 5/10 ms Opus packet, one frame, stereo.
				config := packet[0] >> 3
				if config < 16 || packet[0]&3 != 0 || packet[0]&4 == 0 {
					e.Close()
					t.Fatalf("unexpected Opus TOC %02x", packet[0])
				}
				samples := 120 << (config & 3)
				if samples != duration*48 {
					e.Close()
					t.Fatalf("samples=%d duration=%d", samples, duration)
				}
			}
		}
		e.Close()
		if count == 0 {
			t.Fatal("no packets")
		}
	}
}
