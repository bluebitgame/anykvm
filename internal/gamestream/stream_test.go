//go:build cgo

package gamestream

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"anykvm/internal/gamestream/native"
)

func TestVideoPacketLossRecovery(t *testing.T) {
	for _, n := range []int{100, 3000, 240000, 700000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			au := make([]byte, n)
			for i := range au {
				au[i] = byte(i * 37)
			}
			v := videoPacketizer{}
			packets, err := v.pack(au, true, 9000, 1024, 2)
			if err != nil {
				t.Fatal(err)
			}
			var recovered []byte
			for off := 0; off < len(packets); {
				info := binary.LittleEndian.Uint32(packets[off][28:])
				ds := int(info >> 22)
				pct := int(info >> 4 & 255)
				ps := (ds*pct + 99) / 100
				size := len(packets[off])
				count := ds + ps
				shards := make([]byte, count*size)
				for i := 0; i < count; i++ {
					copy(shards[i*size:], packets[off+i])
					if binary.BigEndian.Uint16(packets[off+i][2:]) != uint16(off+i) {
						t.Fatal("RTP sequence")
					}
				}
				marks := make([]byte, count)
				marks[0] = 1
				clear(shards[:size])
				if ds > 1 {
					marks[ds-1] = 1
					clear(shards[(ds-1)*size : ds*size])
				}
				if err := native.Code(shards, ds, ps, size, false, marks); err != nil {
					t.Fatal(err)
				}
				for i := 0; i < ds; i++ {
					recovered = append(recovered, shards[i*size+32:(i+1)*size]...)
				}
				off += count
			}
			if recovered[0] != 1 || recovered[3] != 2 || !bytes.Equal(recovered[8:8+n], au) {
				t.Fatal("lost video payload not recovered")
			}
			if got := binary.LittleEndian.Uint16(recovered[4:]); got != uint16((n+7)%1008+1) {
				t.Fatal("last payload length", got)
			}
		})
	}
}
func TestEncryptedVideoAndAudio(t *testing.T) {
	key := bytes.Repeat([]byte{19}, 16)
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	v := videoPacketizer{gcm: gcm}
	packets, err := v.pack([]byte{0, 0, 0, 1, 0x65, 9}, true, 45, 1024, 2)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range packets {
		plain, err := gcm.Open(nil, p[:12], append(append([]byte{}, p[32:]...), p[16:32]...), nil)
		if err != nil {
			t.Fatal(err)
		}
		if plain[0] != 0x90 || binary.LittleEndian.Uint64(p[:8]) != uint64(i) {
			t.Fatal("video encryption header")
		}
	}
	a := audioPacketizer{block: block, keyID: 42, duration: 5}
	var data [][]byte
	var fec [][]byte
	for i := 0; i < 4; i++ {
		opus := bytes.Repeat([]byte{byte(i + 1)}, 80)
		ps, err := a.pack(opus)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, ps[0])
		if i == 3 {
			fec = ps[1:]
		}
		iv := make([]byte, 16)
		binary.BigEndian.PutUint32(iv, 42+uint32(i))
		out := append([]byte{}, ps[0][12:]...)
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, out)
		if !bytes.Equal(out[:80], opus) || out[80] != 16 {
			t.Fatal("audio CBC payload")
		}
		if binary.BigEndian.Uint32(ps[0][4:]) != uint32(i*5) {
			t.Fatal("audio timestamp")
		}
	}
	size := len(data[0]) - 12
	shards := make([]byte, 6*size)
	for i, p := range data {
		copy(shards[i*size:], p[12:])
	}
	for i, p := range fec {
		if p[1] != 127 || binary.BigEndian.Uint16(p[14:]) != 0 {
			t.Fatal("audio FEC header")
		}
		copy(shards[(i+4)*size:], p[24:])
	}
	clear(shards[:size*2])
	if err := native.Code(shards, 4, 2, size, true, []byte{1, 1, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(shards[:size], data[0][12:]) || !bytes.Equal(shards[size:2*size], data[1][12:]) {
		t.Fatal("audio recovery")
	}
}

type tinyReader struct{ io.Reader }

func (r tinyReader) Read(p []byte) (int, error) { return r.Reader.Read(p[:min(1, len(p))]) }
func TestAccessUnitsAcrossPipeReads(t *testing.T) {
	a := []byte{0, 0, 0, 1, 9, 0xf0, 0, 0, 1, 0x67, 2, 0, 0, 1, 0x65, 3, 0, 0, 1, 0x65, 4}
	b := []byte{0, 0, 1, 9, 0xf0, 0, 0, 1, 0x41, 8}
	var got [][]byte
	err := readAccessUnits(tinyReader{bytes.NewReader(append(append([]byte{}, a...), b...))}, func(p []byte) error { got = append(got, append([]byte{}, p...)); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !bytes.Equal(got[0], a) || !bytes.Equal(got[1], b) {
		t.Fatalf("AU split: %x", got)
	}
}
func TestRTSPRejectsBadFraming(t *testing.T) {
	for _, s := range []string{"OPTIONS * HTTP/1.1\r\nCSeq: 1\r\n\r\n", "ANNOUNCE * RTSP/1.0\r\nCSeq: 1\r\nContent-Length: -1\r\n\r\n", "OPTIONS * RTSP/1.0\r\nCSeq: 1\r\nCSeq: 2\r\n\r\n"} {
		if _, err := parseRTSP(strings.NewReader(s)); err == nil {
			t.Fatal("accepted malformed RTSP")
		}
	}
}

type testHID struct {
	mu     sync.Mutex
	events []string
}

func (h *testHID) add(s string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, s)
	return nil
}
func (h *testHID) KeyDown(k uint8) error       { return h.add(fmt.Sprintf("down:%d", k)) }
func (h *testHID) KeyRelease(k uint8) error    { return h.add(fmt.Sprintf("up:%d", k)) }
func (h *testHID) KeyUp() error                { return h.add("upall") }
func (h *testHID) MouseMove(x, y int) error    { return h.add(fmt.Sprintf("abs:%d,%d", x, y)) }
func (h *testHID) MouseMoveRel(x, y int) error { return h.add(fmt.Sprintf("rel:%d,%d", x, y)) }
func (h *testHID) MouseButton(b int, down bool) error {
	return h.add(fmt.Sprintf("button:%d,%t", b, down))
}
func (h *testHID) MouseWheel(n int) error { return h.add(fmt.Sprintf("wheel:%d", n)) }
func (h *testHID) has(s string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, v := range h.events {
		if v == s {
			return true
		}
	}
	return false
}
func keyInput() []byte {
	p := make([]byte, 14)
	binary.BigEndian.PutUint32(p, 10)
	binary.LittleEndian.PutUint32(p[4:], 3)
	binary.LittleEndian.PutUint16(p[9:], 'A')
	return p
}
func clientControl(key []byte, seq uint32, kind uint16, payload []byte) []byte {
	b, _ := aes.NewCipher(key)
	a, _ := cipher.NewGCM(b)
	plain := make([]byte, 4+len(payload))
	binary.LittleEndian.PutUint16(plain, kind)
	binary.LittleEndian.PutUint16(plain[2:], uint16(len(payload)))
	copy(plain[4:], payload)
	iv := make([]byte, 12)
	binary.LittleEndian.PutUint32(iv, seq)
	iv[10] = 'C'
	iv[11] = 'C'
	sealed := a.Seal(nil, iv, plain, nil)
	out := make([]byte, 24+len(plain))
	binary.LittleEndian.PutUint16(out, 1)
	binary.LittleEndian.PutUint16(out[2:], uint16(len(out)-4))
	binary.LittleEndian.PutUint32(out[4:], seq)
	copy(out[8:24], sealed[len(plain):])
	copy(out[24:], sealed[:len(plain)])
	return out
}
func TestControlAuthenticationAndReplay(t *testing.T) {
	key := make([]byte, 16)
	a, _ := controlAEAD(key, true)
	p := clientControl(key, 2, 0x206, keyInput())
	kind, seq, payload, err := openControl(a, true, p)
	if err != nil || kind != 0x206 || seq != 2 || !bytes.Equal(payload, keyInput()) {
		t.Fatal("control decode", err)
	}
	p[8] ^= 1
	if _, _, _, err := openControl(a, true, p); err == nil {
		t.Fatal("accepted corrupt tag")
	}
	var w replayWindow
	for _, s := range []uint32{2, 1, 4098, 4097} {
		if !w.accept(s) {
			t.Fatal("fresh seq rejected")
		}
	}
	if w.accept(4097) || w.accept(1) {
		t.Fatal("replay accepted")
	}
}

func encryptedRTSP(t *testing.T, s *Server, key []byte, seq uint32, method, target, body string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", s.rtspPort), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	plain := []byte(fmt.Sprintf("%s %s RTSP/1.0\r\nCSeq: %d\r\nContent-Length: %d\r\n\r\n%s", method, target, seq, len(body), body))
	b, _ := aes.NewCipher(key)
	g, _ := cipher.NewGCM(b)
	iv := make([]byte, 12)
	binary.LittleEndian.PutUint32(iv, seq)
	iv[10] = 'C'
	iv[11] = 'R'
	sealed := g.Seal(nil, iv, plain, nil)
	out := make([]byte, 24+len(plain))
	binary.BigEndian.PutUint32(out, uint32(len(plain))|0x80000000)
	binary.BigEndian.PutUint32(out[4:], seq)
	copy(out[8:24], sealed[len(plain):])
	copy(out[24:], sealed[:len(plain)])
	if _, err = conn.Write(out); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(response) < 24 {
		t.Fatal("short RTSP reply")
	}
	n := int(binary.BigEndian.Uint32(response) & 0x7fffffff)
	if n+24 != len(response) {
		t.Fatal("RTSP length")
	}
	binary.LittleEndian.PutUint32(iv, binary.BigEndian.Uint32(response[4:]))
	iv[10] = 'H'
	opened, err := g.Open(nil, iv, append(append([]byte{}, response[24:]...), response[8:24]...), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(opened, []byte("RTSP/1.0 200")) {
		t.Fatalf("RTSP response: %s", opened)
	}
	return string(opened)
}

func TestStreamingLifecycle(t *testing.T) {
	ff := os.Getenv("ANYKVM_TEST_FFMPEG")
	if ff == "" {
		t.Skip("set ANYKVM_TEST_FFMPEG for real H.264/Opus and ENet integration")
	}
	if _, err := os.Stat(ff); err != nil {
		t.Fatal(err)
	}
	var starts, stops atomic.Int32
	hid := &testHID{}
	s, err := NewServer(Config{HTTPAddr: "127.0.0.1:0", HTTPSAddr: "127.0.0.1:0", StateFile: filepath.Join(t.TempDir(), "identity.json"), Machines: []string{"Test"}, MachineAudio: []bool{true}, RequestPIN: func(context.Context, PairingRequest) (string, error) { return "1234", nil }, HID: hid, OnStreamStart: func(int, capture.VideoStreamOptions) error { starts.Add(1); return nil }, OnStreamStop: func() { stops.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := pairClient(t, s)
	client := tlsClient(t, s, &c.cert)
	q := url.Values{"appid": {fmt.Sprint(s.apps[0].ID)}, "rikey": {"00000000000000000000000000000000"}, "rikeyid": {"-2147483648"}, "corever": {"1"}}
	r := getReply(t, client, secureURL(s), "/launch", q)
	if r.Code != 200 {
		t.Fatal("launch", r)
	}
	x := s.currentSession()
	if x.keyID != 0x80000000 {
		t.Fatalf("signed key ID lost bits: %#x", x.keyID)
	}
	key := make([]byte, 16)
	encryptedRTSP(t, s, key, 1, "OPTIONS", "*", "")
	encryptedRTSP(t, s, key, 2, "DESCRIBE", "rtsp://127.0.0.1", "")
	for i, kind := range []string{"audio", "video", "control"} {
		encryptedRTSP(t, s, key, uint32(3+i), "SETUP", "streamid="+kind, "")
	}
	sdp := "a=x-nv-video[0].clientViewportWd:320\r\na=x-nv-video[0].clientViewportHt:240\r\na=x-nv-video[0].maxFPS:30\r\na=x-nv-vqos[0].bw.maximumBitrateKbps:2000\r\na=x-ss-general.encryptionEnabled:7\r\na=x-ml-general.featureFlags:2\r\n"
	encryptedRTSP(t, s, key, 6, "ANNOUNCE", "streamid=video", sdp)
	video, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer video.Close()
	audio, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Close()
	ping := append([]byte(x.ping), 0, 0, 0, 1)
	video.WriteToUDP(ping, s.videoSocket.LocalAddr().(*net.UDPAddr))
	audio.WriteToUDP(ping, s.audioSocket.LocalAddr().(*net.UDPAddr))
	enet, err := native.NewHost("127.0.0.1", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer enet.Close()
	if err = enet.Dial("127.0.0.1", s.controlPort, x.connectData); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	connected := false
	for time.Now().Before(deadline) {
		e, err := enet.Poll(5)
		if err != nil {
			t.Fatal(err)
		}
		if e.Type == native.Connect {
			connected = true
			break
		}
	}
	if !connected {
		t.Fatal("ENet handshake timed out")
	}
	encryptedRTSP(t, s, key, 7, "PLAY", "/", "")
	f := bridge.Frame{Format: capture.FormatRaw, Width: 320, Height: 240, Data: make([]byte, 320*240*4)}
	s.PushFrame(f)
	// Feed configured stereo PCM; codecs may buffer their first input packet.
	s.PushAudio(capture.AudioFrame{SampleRate: 48000, Channels: 2, PCM: make([]byte, 20*48*x.options.AudioDuration*4)})
	if err = enet.Send(0, clientControl(key, 1, 0x206, keyInput())); err != nil {
		t.Fatal(err)
	}
	// Read and independently reassemble the first encrypted video frame.
	video.SetReadDeadline(time.Now().Add(12 * time.Second))
	var au []byte
	buf := make([]byte, 2048)
	for {
		n, _, err := video.ReadFromUDP(buf)
		if err != nil {
			t.Fatal("video", err)
		}
		p := buf[:n]
		plain, err := x.gcm.Open(nil, p[:12], append(append([]byte{}, p[32:]...), p[16:32]...), nil)
		if err != nil {
			t.Fatal(err)
		}
		info := binary.LittleEndian.Uint32(plain[28:])
		shard := int(info >> 12 & 1023)
		ds := int(info >> 22)
		if shard >= ds {
			continue
		}
		au = append(au, plain[32:]...)
		if plain[24]&2 != 0 {
			last := int(binary.LittleEndian.Uint16(au[4:]))
			au = au[:len(au)-1008+last]
			break
		}
	}
	if !hasNAL(au[8:], 5) || !hasNAL(au[8:], 7) {
		t.Fatal("first video frame missing SPS/IDR")
	}
	decode := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-f", "h264", "-i", "pipe:0", "-frames:v", "1", "-f", "rawvideo", "-pix_fmt", "bgra", "pipe:1")
	decode.Stdin = bytes.NewReader(au[8:])
	raw, err := decode.Output()
	if err != nil || len(raw) != 320*240*4 {
		t.Fatalf("decode video: %v bytes=%d", err, len(raw))
	}
	audio.SetReadDeadline(time.Now().Add(8 * time.Second))
	n, _, err := audio.ReadFromUDP(buf)
	if err != nil {
		t.Fatal("audio", err)
	}
	if n < 28 || buf[1] != 97 {
		t.Fatal("missing Opus RTP")
	}
	deadline = time.Now().Add(time.Second)
	for !hid.has("down:4") && time.Now().Before(deadline) {
		enet.Poll(5)
	}
	if !hid.has("down:4") {
		t.Fatal("keyboard not delivered")
	}
	if r = getReply(t, client, secureURL(s), "/cancel", nil); r.Code != 200 {
		t.Fatal(r)
	}
	if starts.Load() != 1 || stops.Load() != 1 || !hid.has("up:4") {
		t.Fatalf("lifecycle %d/%d release=%v", starts.Load(), stops.Load(), hid.has("up:4"))
	}
	if s.currentSession() != nil {
		t.Fatal("session not cleared")
	}
	if r = getReply(t, http.DefaultClient, plainURL(s), "/serverinfo", nil); r.Game != "0" {
		t.Fatal("still busy")
	}
}

func TestJPEGEncoderForceKeyframe(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_AVMEDIA") == "" {
		t.Skip("set ANYKVM_TEST_AVMEDIA for in-process encoding")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	socket, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	var jpg bytes.Buffer
	if err = jpeg.Encode(&jpg, image.NewRGBA(image.Rect(0, 0, 320, 240)), nil); err != nil {
		t.Fatal(err)
	}
	f := bridge.Frame{Format: capture.FormatJPEG, Width: 320, Height: 240, Data: jpg.Bytes()}
	x := &streamSession{server: &Server{videoSocket: socket}, ctx: ctx, options: streamOptions{Width: 320, Height: 240, FPS: 30, Bitrate: 2000, PacketSize: 1024, MinFEC: 2}, videoPeer: peer.LocalAddr().(*net.UDPAddr), frames: make(chan bridge.Frame, 1), keyframe: make(chan struct{}, 1)}
	done := make(chan error, 1)
	v := &videoPacketizer{}
	go func() { _, err := x.encodeVideo(f, v, time.Now()); done <- err }()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	peer.SetReadDeadline(time.Now().Add(4 * time.Second))
	buf := make([]byte, 2048)
	n, _, err := peer.ReadFromUDP(buf)
	if err != nil {
		t.Fatal("JPEG encoder produced no video", err)
	}
	if n < 40 || buf[35] != 2 {
		t.Fatal("JPEG stream did not begin with IDR")
	}
	// A keyframe request keeps the same encoder and packetizer alive.
	previous := binary.LittleEndian.Uint32(buf[20:])
	x.keyframe <- struct{}{}
	peer.SetReadDeadline(time.Now().Add(4 * time.Second))
	found := false
	for !found {
		n, _, err = peer.ReadFromUDP(buf)
		if err != nil {
			t.Fatal("forced keyframe", err)
		}
		if n >= 40 && binary.LittleEndian.Uint32(buf[20:]) > previous+10 {
			t.Fatal("keyframe request was not honored before the periodic GOP")
		}
		if n >= 40 && binary.LittleEndian.Uint32(buf[20:]) > previous && buf[24]&4 != 0 && buf[35] == 2 {
			found = true
		}
	}
}
