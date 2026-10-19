//go:build windows && !ffmpeg

package gamestream

import (
	"context"
	"os"
	"testing"
)

func TestMissingOpusDLLPreservesSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	x := &streamSession{server: &Server{cfg: Config{MachineAudio: []bool{true}}}, ctx: ctx}
	err := x.nativeAudioLoop(func() (nativeOpusEncoder, error) {
		api, err := loadWindowsOpus([]string{t.TempDir()})
		if err != nil {
			return nil, err
		}
		return newDLLOpusEncoder(5, api)
	})
	if err == nil || ctx.Err() != nil {
		t.Fatalf("missing Opus DLL did not disable only audio: %v, %v", err, ctx.Err())
	}
}

// Run on Windows with ANYKVM_TEST_OPUS_DLL=1 and ANYKVM_OPUS_LIBDIR set to the DLL directory.
func TestWindowsOpusDLL(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_OPUS_DLL") != "1" {
		t.Skip("set ANYKVM_TEST_OPUS_DLL=1 to exercise the prebuilt DLL")
	}
	api, err := loadWindowsOpus([]string{os.Getenv("ANYKVM_OPUS_LIBDIR")})
	if err != nil {
		t.Fatal(err)
	}
	for _, duration := range []int{5, 10, 20} {
		enc, err := newDLLOpusEncoder(duration, api)
		if err != nil {
			t.Fatal(err)
		}
		for frame := 0; frame < 10; frame++ {
			pcm := make([]byte, 48*duration*4)
			if frame%2 != 0 {
				for i := range pcm {
					pcm[i] = byte(i*13 + frame)
				}
			}
			packet, err := enc.Encode(pcm)
			if err != nil || len(packet) != duration*16 {
				t.Fatalf("duration=%d frame=%d packet=%d err=%v", duration, frame, len(packet), err)
			}
		}
		enc.Close()
	}
}
