//go:build !ffmpeg

package gamestream

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
)

const (
	opusApplicationLowDelay = 2051
	opusSetBitrate          = 4002
	opusSetVBR              = 4006
)

// opusFunctions isolates the C ABI from PCM framing and encoder ownership.
type opusFunctions struct {
	create  func(rate, channels, application int) (uintptr, int32)
	control func(encoder uintptr, request, value int) int32
	encode  func(encoder uintptr, pcm []int16, samples int, packet []byte) int32
	destroy func(encoder uintptr)
}

type dllOpusEncoder struct {
	mu          sync.Mutex
	api         *opusFunctions
	encoder     uintptr
	pcm         []int16
	packetBytes int
}

func newDLLOpusEncoder(duration int, api *opusFunctions) (*dllOpusEncoder, error) {
	if duration != 5 && duration != 10 && duration != 20 {
		return nil, fmt.Errorf("Opus requires 5, 10 or 20 ms packets, got %d", duration)
	}
	encoder, code := api.create(48000, 2, opusApplicationLowDelay)
	if code != 0 || encoder == 0 {
		if encoder != 0 {
			api.destroy(encoder)
		}
		return nil, fmt.Errorf("opus_encoder_create failed: %d", code)
	}
	e := &dllOpusEncoder{api: api, encoder: encoder, pcm: make([]int16, duration*48*2), packetBytes: opusBitrateKbps * duration / 8}
	// Moonlight audio FEC requires equally sized packets. Disable VBR and leave
	// DTX disabled, including when the captured PCM contains silence.
	for _, option := range [][2]int{{opusSetBitrate, opusBitrateKbps * 1000}, {opusSetVBR, 0}} {
		if code := api.control(encoder, option[0], option[1]); code != 0 {
			e.Close()
			return nil, fmt.Errorf("opus_encoder_ctl(%d) failed: %d", option[0], code)
		}
	}
	return e, nil
}

func (e *dllOpusEncoder) Encode(data []byte) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.encoder == 0 {
		return nil, errors.New("Opus encoder is closed")
	}
	if len(data) != len(e.pcm)*2 {
		return nil, fmt.Errorf("Opus requires %d PCM bytes, got %d", len(e.pcm)*2, len(data))
	}
	// Capture buffers need not be aligned for int16 reads. Decode little-endian
	// samples into aligned storage before passing them through the native ABI.
	for i := range e.pcm {
		e.pcm[i] = int16(binary.LittleEndian.Uint16(data[2*i:]))
	}
	packet := make([]byte, e.packetBytes)
	n := e.api.encode(e.encoder, e.pcm, len(e.pcm)/2, packet)
	if n < 0 {
		return nil, fmt.Errorf("opus_encode failed: %d", n)
	}
	if int(n) != len(packet) {
		return nil, fmt.Errorf("Opus CBR packet has %d bytes, expected %d", n, len(packet))
	}
	return packet, nil
}

func (e *dllOpusEncoder) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.encoder != 0 {
		e.api.destroy(e.encoder)
		e.encoder = 0
	}
}
