package airplay

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha512"
	"encoding/binary"
	"net"
	"testing"

	"anykvm/internal/airplay/playfair"
	"howett.net/plist"
)

func TestLegacyPairedALACWithoutMirror(t *testing.T) {
	if err := playfair.Available(); err != nil {
		t.Skip(err)
	}
	r, err := NewReceiver("airplay:Audio-Only", Options{})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.start(l, nil)
	defer r.Close()
	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sender := &testSender{conn: conn, reader: bufio.NewReader(conn)}
	status, info := sender.exchange(t, "GET", "/info", nil)
	var caps struct {
		Formats []struct {
			Output uint64 `plist:"audioOutputFormats"`
		} `plist:"audioFormats"`
	}
	if _, err := plist.Unmarshal(info, &caps); err != nil || status != 200 || len(caps.Formats) != 2 || caps.Formats[0].Output != supportedAudioFormats || caps.Formats[1].Output != supportedAudioFormats {
		t.Fatal("ALAC/AAC-ELD advertisement", caps, status, err)
	}
	secret := sender.pair(t)
	message := sender.fairplay(t)
	timing, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer timing.Close()
	ek, iv := make([]byte, 72), bytes.Repeat([]byte{3}, 16)
	for i := range ek {
		ek[i] = byte(255 - i)
	}
	body, _ := plist.Marshal(map[string]any{"ekey": ek, "eiv": iv, "timingPort": uint64(timing.LocalAddr().(*net.UDPAddr).Port), "timingProtocol": "NTP",
		"streams": []any{map[string]any{"type": uint64(96), "ct": uint64(2), "audioFormat": alacAudioFormat, "spf": uint64(352), "sr": uint64(44100), "usingScreen": false}}}, plist.BinaryFormat)
	status, response := sender.exchange(t, "SETUP", "/stream", body)
	var setup struct {
		Streams []struct {
			Type        uint64 `plist:"type"`
			DataPort    uint64 `plist:"dataPort"`
			ControlPort uint64 `plist:"controlPort"`
		} `plist:"streams"`
	}
	if _, err := plist.Unmarshal(response, &setup); err != nil || status != 200 || len(setup.Streams) != 1 || setup.Streams[0].Type != 96 || setup.Streams[0].DataPort == 0 || setup.Streams[0].ControlPort == 0 {
		t.Fatal("standalone audio SETUP", status, setup, err)
	}
	if status, _ := sender.exchange(t, "RECORD", "/stream", nil); status != 200 {
		t.Fatal("audio RECORD", status)
	}
	audio := (&Device{receiver: r}).AudioDevice()
	if err := audio.Open(); err != nil {
		t.Fatal(err)
	}
	defer audio.Close()
	udp, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: int(setup.Streams[0].DataPort)})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	key, err := playfair.Decrypt(message, ek)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha512.Sum512(append(key, secret...))
	block, _ := aes.NewCipher(hash[:16])
	payload := bytes.Repeat([]byte{0x20, 1, 2, 3}, 9)
	packet := make([]byte, 12+len(payload))
	packet[0], packet[1] = 0x80, 0x60
	binary.BigEndian.PutUint16(packet[2:], 1)
	copy(packet[12:], payload)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(packet[12:44], payload[:32])
	if _, err := udp.Write(packet); err != nil {
		t.Fatal(err)
	}
	frame, err := audio.Capture()
	if err != nil || frame == nil || frame.Codec != "alac" || frame.FramesPerPacket != 352 || !bytes.Equal(frame.Encoded, payload) {
		t.Fatal("ALAC without mirror delivery", frame, err)
	}
	if len(r.frames) != 0 {
		t.Fatal("audio-only playback produced video")
	}
	if status, _ := sender.exchange(t, "TEARDOWN", "/stream", nil); status != 200 {
		t.Fatal("audio teardown", status)
	}
}
