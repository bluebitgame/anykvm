package wsmedia

import (
	"anykvm/internal/capture"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"
)

func TestEncodedAudioCapabilitiesAndWireIdentity(t *testing.T) {
	s := NewServer(Config{})
	c := &wsClient{active: true}
	s.clients[c] = struct{}{}
	f := capture.AudioFrame{Encoded: []byte{1, 2, 3, 4}, Codec: "mp4a.40.39", Config: []byte{0xf8, 0xe6, 0x50, 0}, SampleRate: 48000, Channels: 2, FramesPerPacket: 480, Timestamp: 10000}
	s.handleHello([]byte(`{"h264":true,"audioCodecs":["mp4a.40.2"]}`), c)
	if s.SupportsEncodedAudio(f) {
		t.Fatal("AAC-LC support accepted AAC-ELD")
	}
	s.handleHello([]byte(`{"h264":true,"audioCodecs":["mp4a.40.39"]}`), c)
	if !s.SupportsEncodedAudio(f) {
		t.Fatal("AAC-ELD passthrough refused")
	}
	msg := encodeAudio(f)
	n := int(binary.BigEndian.Uint16(msg[1:]))
	var meta audioMetadata
	if err := json.Unmarshal(msg[3:3+n], &meta); err != nil {
		t.Fatal(err)
	}
	if msg[0] != msgEncodedAudio || meta.Codec != f.Codec || !bytes.Equal(meta.Config, f.Config) || meta.Timestamp != f.Timestamp || !bytes.Equal(msg[3+n:], f.Encoded) {
		t.Fatal("compressed payload or decoder config changed")
	}
	f.Config = []byte{1}
	if s.SupportsEncodedAudio(f) {
		t.Fatal("untested config accepted")
	}
	f.Config = []byte{0xf8, 0xe6, 0x50, 0}
	s.clients[&wsClient{active: true}] = struct{}{}
	if s.SupportsEncodedAudio(f) {
		t.Fatal("incompatible second client accepted")
	}
}

func TestEncodedAudioExactRateNegotiation(t *testing.T) {
	s := NewServer(Config{})
	c := &wsClient{active: true}
	s.clients[c] = struct{}{}
	f := capture.AudioFrame{Codec: "mp4a.40.39", Config: []byte{0xf8, 0xe8, 0x50, 0}, SampleRate: 44100, Channels: 2, FramesPerPacket: 480}
	s.handleHello([]byte(`{"audioCodecs":["mp4a.40.39"]}`), c)
	if s.SupportsEncodedAudio(f) {
		t.Fatal("legacy 48kHz probe accepted AirPlay 44.1kHz")
	}
	s.handleHello([]byte(`{"audioFormats":[{"codec":"mp4a.40.39","sampleRate":48000,"channels":2,"config":"+OZQAA==","frames":480}]}`), c)
	if s.SupportsEncodedAudio(f) {
		t.Fatal("48kHz probe accepted 44.1kHz")
	}
	s.handleHello([]byte(`{"audioFormats":[{"codec":"mp4a.40.39","sampleRate":44100,"channels":2,"config":"+OhQAA==","frames":480}]}`), c)
	if !s.SupportsEncodedAudio(f) {
		t.Fatal("exact AirPlay probe rejected")
	}
	f.SampleRate = 48000
	f.Config = []byte{0xf8, 0xe6, 0x50, 0}
	if s.SupportsEncodedAudio(f) {
		t.Fatal("44.1kHz probe accepted CoreDevice 48kHz")
	}
	s.handleHello([]byte(`{"audioFormats":[],"audioCodecs":["mp4a.40.39"]}`), c)
	if s.SupportsEncodedAudio(f) {
		t.Fatal("empty exact support fell back to codec-only support")
	}
}

func TestAudioAvailabilityMetadata(t *testing.T) {
	s := NewServer(Config{})
	if s.meta.AudioAvailable {
		t.Fatal("audio initially available")
	}
	s.SetAudioAvailable(true)
	if !s.meta.AudioAvailable {
		t.Fatal("audio readiness not advertised")
	}
	s.SetAudioAvailable(false)
	if s.meta.AudioAvailable {
		t.Fatal("audio stop not advertised")
	}
}
