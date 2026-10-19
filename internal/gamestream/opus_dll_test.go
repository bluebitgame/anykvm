//go:build !ffmpeg

package gamestream

import (
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
	"time"

	"anykvm/internal/capture"
)

func fakeOpus() *opusFunctions {
	return &opusFunctions{
		create:  func(int, int, int) (uintptr, int32) { return 1, 0 },
		control: func(uintptr, int, int) int32 { return 0 },
		encode:  func(_ uintptr, _ []int16, _ int, packet []byte) int32 { return int32(len(packet)) },
		destroy: func(uintptr) {},
	}
}

func TestDLLOpusPCMAndCBR(t *testing.T) {
	for _, duration := range []int{5, 10, 20} {
		t.Run((time.Duration(duration) * time.Millisecond).String(), func(t *testing.T) {
			api := fakeOpus()
			var controls [][2]int
			api.create = func(rate, channels, application int) (uintptr, int32) {
				if rate != 48000 || channels != 2 || application != opusApplicationLowDelay {
					t.Fatalf("unexpected Opus configuration: %d/%d/%d", rate, channels, application)
				}
				return 1, 0
			}
			api.control = func(_ uintptr, request, value int) int32 {
				controls = append(controls, [2]int{request, value})
				return 0
			}
			count := 0
			api.encode = func(_ uintptr, pcm []int16, samples int, packet []byte) int32 {
				if samples != duration*48 || len(pcm) != samples*2 {
					t.Fatalf("incorrect frame size: %d / %d", samples, len(pcm))
				}
				for i, sample := range pcm {
					if sample != int16(i*97-32768) {
						t.Fatalf("PCM sample %d: %d", i, sample)
					}
				}
				count++
				packet[0] = byte(count)
				return int32(len(packet))
			}
			enc, err := newDLLOpusEncoder(duration, api)
			if err != nil {
				t.Fatal(err)
			}
			defer enc.Close()
			if !reflect.DeepEqual(controls, [][2]int{{4002, 128000}, {4006, 0}}) {
				t.Fatalf("missing bitrate/CBR controls: %v", controls)
			}
			// Deliberately misalign the incoming byte buffer.
			pcm := make([]byte, duration*48*4+1)[1:]
			for i := 0; i < len(pcm)/2; i++ {
				binary.LittleEndian.PutUint16(pcm[2*i:], uint16(int16(i*97-32768)))
			}
			first, err := enc.Encode(pcm)
			if err != nil || len(first) != duration*16 {
				t.Fatalf("CBR packet: %d bytes, %v", len(first), err)
			}
			if _, err := enc.Encode(pcm); err != nil {
				t.Fatal(err)
			}
			if first[0] != 1 {
				t.Fatal("next encode overwrote the previous packet")
			}
		})
	}
}

func TestDLLOpusErrorsAndOwnership(t *testing.T) {
	api := fakeOpus()
	for _, duration := range []int{0, 2, 40} {
		if _, err := newDLLOpusEncoder(duration, api); err == nil {
			t.Fatalf("accepted unsupported duration %d", duration)
		}
	}
	api.create = func(int, int, int) (uintptr, int32) { return 0, -7 }
	if _, err := newDLLOpusEncoder(5, api); err == nil {
		t.Fatal("ignored allocation failure")
	}
	api = fakeOpus()
	closed := 0
	api.destroy = func(uintptr) { closed++ }
	api.control = func(uintptr, int, int) int32 { return -1 }
	if _, err := newDLLOpusEncoder(5, api); err == nil || closed != 1 {
		t.Fatalf("initialization failure leaked encoder: %v, closed=%d", err, closed)
	}
	api.control = func(uintptr, int, int) int32 { return 0 }
	enc, err := newDLLOpusEncoder(5, api)
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{0, 959, 961} {
		if _, err := enc.Encode(make([]byte, size)); err == nil {
			t.Fatalf("accepted incorrect PCM length %d", size)
		}
	}
	for _, result := range []int32{-1, 0, 79, 81} {
		api.encode = func(uintptr, []int16, int, []byte) int32 { return result }
		if _, err := enc.Encode(make([]byte, 960)); err == nil {
			t.Fatalf("accepted invalid packet length/error %d", result)
		}
	}
	enc.Close()
	enc.Close()
	if closed != 2 {
		t.Fatalf("destroyed encoder more than once: %d", closed)
	}
	if _, err := enc.Encode(make([]byte, 960)); err == nil {
		t.Fatal("encoded after Close")
	}
}

func TestDLLOpusEncodeFailurePreservesSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	x := &streamSession{server: &Server{cfg: Config{MachineAudio: []bool{true}}}, ctx: ctx, audio: make(chan capture.AudioFrame, 1)}
	x.options.AudioDuration = 5
	x.audio <- capture.AudioFrame{PCM: make([]byte, 960)}
	api := fakeOpus()
	api.encode = func(uintptr, []int16, int, []byte) int32 { return -3 }
	closed := false
	api.destroy = func(uintptr) { closed = true }
	err := x.nativeAudioLoop(func() (nativeOpusEncoder, error) { return newDLLOpusEncoder(5, api) })
	if err == nil || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || !closed {
		t.Fatalf("audio failure affected session or leaked codec: err=%v, ctx=%v, closed=%v", err, ctx.Err(), closed)
	}
}
