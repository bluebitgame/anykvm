package wsmedia

import (
	"anykvm/internal/capture"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
)

type audioMetadata struct {
	Codec      string   `json:"codec"`
	Config     []byte   `json:"config"`
	SampleRate int      `json:"sampleRate"`
	Channels   int      `json:"channels"`
	Timestamp  int64    `json:"timestamp"`
	Frames     int      `json:"frames"`
	Gain       *float64 `json:"gain,omitempty"`
	Epoch      uint64   `json:"epoch,omitempty"`
}

func audioFormatKey(a audioMetadata) string {
	return fmt.Sprintf("%s/%d/%d/%x/%d", a.Codec, a.SampleRate, a.Channels, a.Config, a.Frames)
}

// The client probes these exact configurations. Requiring an exact match avoids
// mistaking AAC-LC support for AAC-ELD or accepting an untested channel layout.
func knownAudioFormat(a audioMetadata) bool {
	if a.Channels != 2 {
		return false
	}
	switch a.Codec {
	case "mp4a.40.39":
		return (a.SampleRate == 48000 && ((bytes.Equal(a.Config, []byte{0xf8, 0xe6, 0x50, 0}) && a.Frames == 480) || (bytes.Equal(a.Config, []byte{0xf8, 0xe6, 0x40, 0}) && a.Frames == 512))) ||
			(a.SampleRate == 44100 && bytes.Equal(a.Config, []byte{0xf8, 0xe8, 0x50, 0}) && a.Frames == 480)
	case "mp4a.40.2":
		return a.SampleRate == 48000 && bytes.Equal(a.Config, []byte{0x11, 0x90}) && a.Frames == 1024
	default:
		return false
	}
}
func audioMeta(a capture.AudioFrame) audioMetadata {
	return audioMetadata{Codec: a.Codec, Config: a.Config, SampleRate: a.SampleRate, Channels: a.Channels, Timestamp: a.Timestamp, Frames: a.FramesPerPacket, Gain: a.Gain, Epoch: a.Epoch}
}
func (s *Server) SupportsEncodedAudio(a capture.AudioFrame) bool {
	meta := audioMeta(a)
	if !knownAudioFormat(meta) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for c := range s.clients {
		if !c.active {
			continue
		}
		found = true
		if c.audioFormats != nil {
			if !c.audioFormats[audioFormatKey(meta)] {
				return false
			}
		} else if a.SampleRate != 48000 || !c.audioCodecs[a.Codec] {
			return false
		}
	}
	return found
}
func encodeAudio(a capture.AudioFrame) []byte {
	meta, _ := json.Marshal(audioMeta(a))
	msg := make([]byte, 3+len(meta)+len(a.Encoded))
	msg[0] = msgEncodedAudio
	binary.BigEndian.PutUint16(msg[1:], uint16(len(meta)))
	copy(msg[3:], meta)
	copy(msg[3+len(meta):], a.Encoded)
	return msg
}
