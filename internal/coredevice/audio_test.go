package coredevice

import (
	"anykvm/internal/capture"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"github.com/google/uuid"
	"howett.net/plist"
	"io"
	"net"
	"testing"
	"time"
)

func TestAudioOfferAndConfig(t *testing.T) {
	data, err := audioOffer(uuid.MustParse("12345678-1234-4234-8234-123456789abc"), 0x11223344)
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]interface{}
	if _, err := plist.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	if p["avcMediaStreamNegotiatorMode"] != uint64(6) {
		t.Fatal(p)
	}
	r, err := zlib.NewReader(bytes.NewReader(p["avcMediaStreamNegotiatorMediaBlob"].([]byte)))
	if err != nil {
		t.Fatal(err)
	}
	blob, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	// Fixed protobuf wire fixture for settings (field 3), not video banks (field 5).
	prefix := []byte{0x08, 1, 0x10, 1, 0x1a, 0x12, 0x08, 0xc4, 0xe6, 0x88, 0x89, 1, 0x10, 0, 0x18, 0, 0x20, 0xff, 0xbc, 1, 0x28, 0, 0x30, 0}
	if !bytes.HasPrefix(blob, prefix) {
		t.Fatalf("audio offer prefix %x", blob[:min(len(blob), len(prefix))])
	}
	cfg := map[string]interface{}{"AudioStreamMode": uint64(8), "RxPayloadType": uint64(101), "SourcePort": uint64(1234), "RemoteSSRC": uint64(10), "LocalSSRC": uint64(20)}
	c, e := audioStreamConfig(cfg)
	if e != nil || c.remoteSSRC != 20 || c.localSSRC != 10 || c.payloadType != 101 {
		t.Fatal(c, e)
	}
	cfg["AudioStreamMode"] = uint64(7)
	if _, e := audioStreamConfig(cfg); e == nil {
		t.Fatal("unknown audio mode mislabeled AAC-ELD")
	}
}
func audioPacket(seq uint16, stamp uint32, payload []byte) []byte {
	p := make([]byte, 12+len(payload))
	p[0] = 0x80
	p[1] = 101
	binary.BigEndian.PutUint16(p[2:], seq)
	binary.BigEndian.PutUint32(p[4:], stamp)
	binary.BigEndian.PutUint32(p[8:], 20)
	copy(p[12:], payload)
	return p
}
func TestAudioRTPHeaderPaddingAndRejection(t *testing.T) {
	cfg := streamConfig{remoteSSRC: 20, payloadType: 101}
	p := audioPacket(1, 10, []byte{1, 2, 3})
	p[0] |= 32
	p = append(p, 0, 0, 3)
	if a, e := audioRTP(p, cfg); e != nil || !bytes.Equal(a, []byte{1, 2, 3}) {
		t.Fatal(a, e)
	}
	for _, bad := range [][]byte{nil, p[:11], append(bytes.Clone(p[:len(p)-1]), 0)} {
		if _, e := audioRTP(bad, cfg); e == nil {
			t.Fatal("malformed RTP accepted")
		}
	}
	p[1] = 100
	if _, e := audioRTP(p, cfg); e == nil {
		t.Fatal("wrong PT accepted")
	}
	p[1] = 101
	p[8] = 1
	if _, e := audioRTP(p, cfg); e == nil {
		t.Fatal("wrong SSRC accepted")
	}
}
func TestAudioUDPReorderWrapAndIndependentClose(t *testing.T) {
	for _, samples := range []int{480, 512} {
		t.Run(fmt.Sprint(samples), func(t *testing.T) { testAudioUDP(t, samples) })
	}
}
func testAudioUDP(t *testing.T, samples int) {
	u, e := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if e != nil {
		t.Fatal(e)
	}
	peer, e := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if e != nil {
		u.Close()
		t.Fatal(e)
	}
	defer peer.Close()
	s := &audioStream{udp: u, peer: peer.LocalAddr().(*net.UDPAddr), config: streamConfig{remoteSSRC: 20, localSSRC: 10, payloadType: 101}, done: make(chan struct{})}
	c := &Client{session: &session{audio: s}}
	d := c.AudioDevice()
	if e := d.Open(); e != nil {
		t.Fatal(e)
	}
	go s.loop()
	defer func() { s.stop(); <-s.done }()
	for _, p := range [][]byte{audioPacket(65535, uint32(0)-uint32(samples), []byte{1}), audioPacket(1, uint32(samples), []byte{3}), audioPacket(0, 0, []byte{2}), audioPacket(0, 0, []byte{99})} {
		if _, e := peer.WriteToUDP(p, u.LocalAddr().(*net.UDPAddr)); e != nil {
			t.Fatal(e)
		}
	}
	var frames []*capture.AudioFrame
	deadline := time.Now().Add(time.Second)
	for len(frames) < 3 && time.Now().Before(deadline) {
		f, e := d.Capture()
		if e != nil {
			t.Fatal(e)
		}
		if f != nil {
			frames = append(frames, f)
		} else {
			time.Sleep(time.Millisecond)
		}
	}
	if len(frames) != 3 {
		t.Fatal("audio lost", len(frames))
	}
	for i, f := range frames {
		config := []byte{0xf8, 0xe6, 0x50, 0}
		if samples == 512 {
			config[2] = 0x40
		}
		if !bytes.Equal(f.Encoded, []byte{byte(i + 1)}) || f.Timestamp != int64(i*samples)*1000000/48000 || f.Codec != "mp4a.40.39" || len(f.PCM) != 0 || f.FramesPerPacket != samples || !bytes.Equal(f.Config, config) {
			t.Fatalf("wrong frame %d: %+v", i, f)
		}
	}
	if e := d.Close(); e != nil {
		t.Fatal(e)
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		t.Fatal("audio unsubscribe closed source session")
	}
	if e := d.Open(); e != nil {
		t.Fatal(e)
	}
	s.stop()
	if _, e := d.Capture(); e == nil {
		t.Fatal("closed source stayed readable")
	}
	d.Close()
}
