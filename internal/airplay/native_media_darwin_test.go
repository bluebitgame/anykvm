//go:build darwin && cgo

package airplay

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha512"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"anykvm/internal/audiodec"
	"anykvm/internal/avmedia"
	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"anykvm/internal/videoenc"
	"anykvm/internal/wsmedia"
	"encoding/json"
	"github.com/gorilla/websocket"
)

func audioFixture(t *testing.T) [][]byte {
	return audioCodecFixture(t, "aaceld")
}

func audioCodecFixture(t *testing.T, codec string) [][]byte {
	t.Helper()
	data, err := os.ReadFile("../testdata/tone." + codec)
	if err != nil {
		t.Fatal(err)
	}
	var packets [][]byte
	for len(data) >= 2 {
		size := int(binary.BigEndian.Uint16(data))
		data = data[2:]
		if size > len(data) {
			t.Fatal("truncated fixture")
		}
		packets = append(packets, data[:size])
		data = data[size:]
	}
	return packets
}

func checkDecodedAudio(t *testing.T, decoder audiodec.Decoder) {
	checkDecodedAudioPackets(t, decoder, audioFixture(t))
}

func checkDecodedAudioPackets(t *testing.T, decoder audiodec.Decoder, packets [][]byte) {
	t.Helper()
	defer decoder.Close()
	var frames, nonzero int
	var previous [2]int16
	var crossings [2]int
	for _, packet := range packets {
		f, err := decoder.Decode(packet)
		if err != nil {
			t.Fatal(err)
		}
		if f == nil {
			continue
		}
		if len(f)%4 != 0 {
			t.Fatal("PCM not stereo aligned")
		}
		frames += len(f) / 4
		for _, b := range f {
			if b != 0 {
				nonzero++
			}
		}
		for i := 0; i+3 < len(f); i += 4 {
			for channel := 0; channel < 2; channel++ {
				sample := int16(binary.LittleEndian.Uint16(f[i+channel*2:]))
				if previous[channel] <= 0 && sample > 0 {
					crossings[channel]++
				}
				previous[channel] = sample
			}
		}
	}
	// 47 AAC-ELD packets * 480 samples at 44.1 kHz = about 24555 output
	// frames at 48 kHz. Permit converter priming, but catch sample-rate or
	// packet-boundary mistakes and all-silent output.
	if frames < 23500 || frames > 25000 || nonzero < 40000 {
		t.Fatalf("invalid PCM: %d frames, %d nonzero bytes", frames, nonzero)
	}
	if crossings[0] < 200 || crossings[0] > 240 || crossings[1] < 310 || crossings[1] > 360 {
		t.Fatalf("wrong sample rate, byte order or channel layout: tone cycles %v", crossings)
	}
}

func TestSystemAirPlayAudioDecode(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_SYSTEM_AIRPLAY") == "" {
		t.Skip("set ANYKVM_TEST_SYSTEM_AIRPLAY for system codec checks")
	}
	decoder, err := audiodec.New(capture.AudioFrame{Codec: "mp4a.40.39", Config: mirrorAudioConfig, SampleRate: 44100, Channels: 2, FramesPerPacket: 480})
	if err != nil {
		t.Fatal(err)
	}
	checkDecodedAudio(t, decoder)
}

func TestSystemAirPlayALACDecode(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_SYSTEM_AIRPLAY") == "" {
		t.Skip("set ANYKVM_TEST_SYSTEM_AIRPLAY for system codec checks")
	}
	options, _ := (audioOptions{CT: 2}).normalized()
	decoder, err := audiodec.New(options.frameFormat())
	if err != nil {
		t.Fatal(err)
	}
	checkDecodedAudioPackets(t, decoder, audioCodecFixture(t, "alac"))
}

func TestLibraryAirPlayALACDecode(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_AVMEDIA") == "" {
		t.Skip("set ANYKVM_TEST_AVMEDIA and ANYKVM_FFMPEG_LIBDIR")
	}
	config := append([]byte{0, 0, 0, 36, 'a', 'l', 'a', 'c', 0, 0, 0, 0}, alacAudioConfig...)
	decoder, err := avmedia.NewAudioDecoder("alac", config, 44100, 2)
	if err != nil {
		t.Fatal(err)
	}
	checkDecodedAudioPackets(t, libraryAudioDecoder{decoder}, audioCodecFixture(t, "alac"))
}

func TestLibraryAirPlayAudioDecode(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_AVMEDIA") == "" {
		t.Skip("set ANYKVM_TEST_AVMEDIA and ANYKVM_FFMPEG_LIBDIR")
	}
	decoder, err := avmedia.NewAudioDecoder("aac", []byte{0xf8, 0xe8, 0x50, 0}, 44100, 2)
	if err != nil {
		t.Fatal(err)
	}
	checkDecodedAudio(t, libraryAudioDecoder{decoder})
}

type libraryAudioDecoder struct{ session *avmedia.Session }

func (d libraryAudioDecoder) Decode(packet []byte) ([]byte, error) {
	f, err := d.session.Decode(packet)
	if err != nil || f == nil {
		return nil, err
	}
	defer f.Release()
	return bytes.Clone(f.Data), nil
}
func (d libraryAudioDecoder) Close() { d.session.Close() }

func TestSystemAirPlayVideoDecodeAndGameStreamEncode(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_SYSTEM_AIRPLAY") == "" {
		t.Skip("set ANYKVM_TEST_SYSTEM_AIRPLAY for system codec checks")
	}
	_, _, au := fixtureAVC(t)
	decoder, err := newMirrorVideoDecoder()
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	if _, ok := decoder.(*systemVideoDecoder); !ok {
		t.Fatal("selected FFmpeg on macOS")
	}
	f, err := decoder.Decode(au)
	if err != nil || f == nil {
		t.Fatalf("system H.264 decode: %v", err)
	}
	if f.Width < 1 || f.Height < 1 || len(f.Data) != f.Width*f.Height*4 {
		t.Fatal("invalid BGRA output")
	}
	// The tiny 64x48 decoder fixture is below hardware encoder size limits.
	// Native pixel transfer scales it to a normal streaming resolution.
	encoder, err := videoenc.New(videoenc.Options{Width: 320, Height: 240, FPS: 30, Bitrate: 2000, Codec: "h264"})
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	if encoder.Name() != "videotoolbox" {
		t.Fatalf("unexpected encoder %s", encoder.Name())
	}
	// Feed exactly the decoded format used by GameStream, including its
	// native encoder's pixel conversion path.
	pixels, ok := encoder.(interface {
		EncodePixels([]byte, int, int, string, bool) ([]byte, error)
	})
	if !ok {
		t.Fatal("native encoder does not accept BGRA")
	}
	encoded, err := pixels.EncodePixels(f.Data, f.Width, f.Height, "bgra", true)
	if err != nil || len(encoded) == 0 {
		t.Fatalf("native GameStream encode: %v", err)
	}
}

func TestSystemAirPlayCaptureWithoutFFmpeg(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_WITHOUT_FFMPEG") == "" {
		t.Skip("set ANYKVM_TEST_WITHOUT_FFMPEG with no loadable FFmpeg libraries")
	}
	if avmedia.Available() == nil {
		t.Fatal("FFmpeg is available; this test must run without it")
	}
	_, _, au := fixtureAVC(t)
	d, err := NewDevice("airplay:No-FFmpeg", Options{})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d.receiver.start(listener, nil)
	if err := d.StartReceiver(1920, 1080, 60); err != nil {
		t.Fatal(err)
	}
	defer d.ShutdownReceiver()
	for _, codec := range []string{"h264", "rawvideo", "mjpeg"} {
		d.SetVideoCodec(codec)
		if err := d.Open(); err != nil {
			t.Fatal(codec, err)
		}
		owner := &pairing{}
		d.receiver.mu.Lock()
		d.receiver.owner = owner
		d.receiver.mu.Unlock()
		d.receiver.frames <- receivedFrame{owner: owner, frame: &capture.Frame{Data: au, Format: capture.FormatH264}}
		frame, err := d.Capture()
		if err != nil || frame == nil {
			t.Fatal(codec, err)
		}
		switch codec {
		case "h264":
			if !bytes.Equal(frame.Data, au) {
				t.Fatal("modified passthrough")
			}
		case "rawvideo":
			if frame.Format != capture.FormatRaw || len(frame.Data) != frame.Width*frame.Height*4 {
				t.Fatal("invalid system pixels")
			}
		case "mjpeg":
			if len(frame.Data) < 2 || frame.Data[0] != 0xff || frame.Data[1] != 0xd8 {
				t.Fatal("invalid ImageIO JPEG")
			}
		}
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
	}
	decoder, err := audiodec.New(capture.AudioFrame{Codec: "mp4a.40.39", Config: mirrorAudioConfig, SampleRate: 44100, Channels: 2, FramesPerPacket: 480})
	if err != nil {
		t.Fatal(err)
	}

	checkDecodedAudio(t, decoder)
}

func TestAirPlayAACToWeb(t *testing.T) {
	d, err := NewDevice("airplay:AAC-Web", Options{})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d.receiver.start(listener, nil)
	defer d.receiver.Close()
	owner := &pairing{}
	d.receiver.mu.Lock()
	d.receiver.owner = owner
	d.receiver.mu.Unlock()
	audio := d.AudioDevice()
	if err := audio.Open(); err != nil {
		t.Fatal(err)
	}
	defer audio.Close()
	s := &mediaSession{receiver: d.receiver, owner: owner, key: make([]byte, 16), secret: make([]byte, 32), audioIV: make([]byte, 16), localIP: net.ParseIP("127.0.0.1"), peerIP: net.ParseIP("127.0.0.1")}
	port, _, err := s.startAudio(audioOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.stopAudio()
	udp, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: s.localIP, Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	// Reserve an ephemeral address, then hand it to the public Web server API.
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	reserved.Close()
	web := wsmedia.NewServer(wsmedia.Config{HTTPAddr: address})
	if err := web.Start(); err != nil {
		t.Fatal(err)
	}
	defer web.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws://"+address+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, message, err := client.ReadMessage(); err != nil || len(message) == 0 || message[0] != 0 {
		t.Fatal("Web metadata", err)
	}
	if err := client.WriteMessage(websocket.BinaryMessage, append([]byte{0x83}, []byte(`{"audioFormats":[{"codec":"mp4a.40.39","sampleRate":44100,"channels":2,"config":"+OhQAA==","frames":480}]}`)...)); err != nil {
		t.Fatal(err)
	}
	hash := sha512.Sum512(make([]byte, 48))
	block, _ := aes.NewCipher(hash[:16])
	for seq, compressed := range audioFixture(t)[:4] {
		packet := make([]byte, 12+len(compressed))
		packet[0], packet[1] = 0x80, 0x60
		binary.BigEndian.PutUint16(packet[2:4], uint16(seq))
		copy(packet[12:], compressed)
		count := len(compressed) / 16 * 16
		cipher.NewCBCEncrypter(block, s.audioIV).CryptBlocks(packet[12:12+count], compressed[:count])
		if _, err := udp.Write(packet); err != nil {
			t.Fatal(err)
		}
	}
	frame, err := audio.Capture()
	if err != nil || frame == nil || len(frame.Encoded) == 0 {
		t.Fatal("AirPlay produced no AAC", err)
	}
	deadline := time.Now().Add(time.Second)
	for !web.SupportsEncodedAudio(*frame) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !web.SupportsEncodedAudio(*frame) {
		t.Fatal("exact AAC negotiation rejected")
	}
	web.PushAudio(*frame)
	_, message, err := client.ReadMessage()
	if err != nil || len(message) < 3 || message[0] != 6 {
		t.Fatal("AirPlay AAC did not reach WebSocket", err)
	}
	n := int(binary.BigEndian.Uint16(message[1:]))
	var meta struct {
		SampleRate int      `json:"sampleRate"`
		Gain       *float64 `json:"gain"`
		Epoch      uint64   `json:"epoch"`
	}
	if err := json.Unmarshal(message[3:3+n], &meta); err != nil {
		t.Fatal(err)
	}
	if meta.SampleRate != 44100 || meta.Gain == nil || *meta.Gain != 1 || meta.Epoch == 0 || !bytes.Equal(message[3+n:], audioFixture(t)[0]) {
		t.Fatal("modified AAC or lost source metadata")
	}
}

// Exercise the real bridge callbacks and system decoder for a browser that
// reports no AAC-ELD support, instead of pushing a frame to WebSocket directly.
func TestSystemAirPlayToWebPCM(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_SYSTEM_AIRPLAY") == "" {
		t.Skip("set ANYKVM_TEST_SYSTEM_AIRPLAY for system codec checks")
	}
	for _, ct := range []uint64{8, 2} {
		t.Run(fmt.Sprint(ct), func(t *testing.T) { testSystemAirPlayToWebPCM(t, ct) })
	}
}

func testSystemAirPlayToWebPCM(t *testing.T, ct uint64) {
	d, err := NewDevice("airplay:PCM-Web", Options{})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d.receiver.start(listener, nil)
	defer d.receiver.Close()
	owner := &pairing{}
	d.receiver.mu.Lock()
	d.receiver.owner = owner
	d.receiver.mu.Unlock()
	s := &mediaSession{receiver: d.receiver, owner: owner, key: make([]byte, 16), secret: make([]byte, 32), audioIV: make([]byte, 16), localIP: net.ParseIP("127.0.0.1"), peerIP: net.ParseIP("127.0.0.1")}
	options, _ := (audioOptions{CT: ct}).normalized()
	port, _, err := s.startAudio(options)
	if err != nil {
		t.Fatal(err)
	}
	defer s.stopAudio()
	b := bridge.NewBridge([]*bridge.MachineSession{{Name: "AirPlay", Capture: d, AudioDev: d.AudioDevice()}}, "alt", 30, 0, 0)
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	reserved.Close()
	web := wsmedia.NewServer(wsmedia.Config{HTTPAddr: address, OnClientConnecting: b.ClaimWS, OnClientConnected: b.OnClientConnected, OnClientDisconnected: b.OnClientDisconnected})
	b.SetWS(web, web)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	if err := web.Start(); err != nil {
		t.Fatal(err)
	}
	defer web.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws://"+address+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := client.WriteMessage(websocket.BinaryMessage, append([]byte{0x83}, []byte(`{"h264":true,"audioFormats":[]}`)...)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		d.receiver.mu.Lock()
		open := d.receiver.audioOpen
		d.receiver.mu.Unlock()
		if open {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Web connection did not start source audio")
		}
		time.Sleep(time.Millisecond)
	}
	udp, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: s.localIP, Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	hash := sha512.Sum512(make([]byte, 48))
	block, _ := aes.NewCipher(hash[:16])
	packets := audioFixture(t)
	if ct == 2 {
		packets = audioCodecFixture(t, "alac")
	}
	for seq, compressed := range packets[:16] {
		packet := make([]byte, 12+len(compressed))
		packet[0], packet[1] = 0x80, 0x60
		binary.BigEndian.PutUint16(packet[2:4], uint16(seq))
		binary.BigEndian.PutUint32(packet[4:8], uint32(seq)*uint32(options.SPF))
		copy(packet[12:], compressed)
		count := len(compressed) / 16 * 16
		cipher.NewCBCEncrypter(block, s.audioIV).CryptBlocks(packet[12:12+count], compressed[:count])
		if _, err := udp.Write(packet); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	available, nonzero := false, 0
	for nonzero == 0 || !available {
		_, message, err := client.ReadMessage()
		if err != nil {
			t.Fatalf("no playable PCM (available=%t nonzero=%d): %v", available, nonzero, err)
		}
		if len(message) == 0 {
			continue
		}
		switch message[0] {
		case 0:
			var meta struct {
				AudioAvailable bool `json:"audioAvailable"`
			}
			if err := json.Unmarshal(message[1:], &meta); err != nil {
				t.Fatal(err)
			}
			available = meta.AudioAvailable
		case 2:
			if (len(message)-1)%4 != 0 {
				t.Fatal("unaligned Web PCM")
			}
			for _, v := range message[1:] {
				if v != 0 {
					nonzero++
				}
			}
		case 6:
			t.Fatal("sent unsupported AAC-ELD instead of PCM")
		}
	}
}
