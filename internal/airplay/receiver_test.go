package airplay

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"anykvm/internal/airplay/playfair"
	"anykvm/internal/capture"
	"howett.net/plist"
)

type testSender struct {
	conn    net.Conn
	reader  *bufio.Reader
	seq     int
	headers http.Header
}

type idleControlConn struct {
	net.Conn
	peer      net.TCPAddr
	data      *strings.Reader
	timeouts  int
	deadlines int
}

func (c *idleControlConn) RemoteAddr() net.Addr            { return &c.peer }
func (c *idleControlConn) SetReadDeadline(time.Time) error { c.deadlines++; return nil }
func (c *idleControlConn) Read(data []byte) (int, error) {
	if c.timeouts > 0 {
		c.timeouts--
		return 0, &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}
	}
	return c.data.Read(data)
}

func TestControlIdleMirrorConnectionsAndPartialRequestDeadline(t *testing.T) {
	wire := "OPTIONS * RTSP/1.0\r\nCSeq: 1\r\n\r\n"
	peer := net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345}
	for _, active := range []bool{false, true} {
		conn := &idleControlConn{peer: peer, data: strings.NewReader(wire), timeouts: 2}
		r := new(Receiver)
		if active {
			r.owner = &pairing{control: conn}
		}
		q, err := r.readControlRequest(conn, bufio.NewReader(conn), time.Second)
		if active && (err != nil || q.method != "OPTIONS" || conn.deadlines != 4) {
			t.Fatalf("active idle connection lost: %+v %v deadlines=%d", q, err, conn.deadlines)
		}
		if !active && err == nil {
			t.Fatal("unowned idle connection did not expire")
		}
	}
	// An active sender must still complete a request within its deadline.
	conn := &idleControlConn{peer: peer, data: strings.NewReader("OPTIONS * RTSP/1.0\r\n")}
	r := &Receiver{owner: &pairing{control: conn}}
	reader := bufio.NewReader(conn)
	reader.Peek(1)
	conn.timeouts = 1
	if _, err := r.readControlRequest(conn, reader, time.Second); err == nil || conn.deadlines != 2 {
		t.Fatalf("partial request was retried: %v deadlines=%d", err, conn.deadlines)
	}
}

func (s *testSender) exchange(t *testing.T, method, path string, body []byte) (int, []byte) {
	t.Helper()
	s.seq++
	s.conn.SetDeadline(time.Now().Add(3 * time.Second))
	wire := fmt.Sprintf("%s %s RTSP/1.0\r\nCSeq: %d\r\nContent-Length: %d\r\n\r\n", method, path, s.seq, len(body))
	if _, err := s.conn.Write(append([]byte(wire), body...)); err != nil {
		t.Fatal(err)
	}
	// http.ReadResponse accepts a normalized RTSP response line; preserve the
	// actual wire protocol check first.
	line, err := s.reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "RTSP/1.0 ") {
		t.Fatal(line)
	}
	combined := io.MultiReader(strings.NewReader(strings.Replace(line, "RTSP/1.0", "HTTP/1.1", 1)), s.reader)
	response, err := http.ReadResponse(bufio.NewReader(combined), nil)
	if err != nil {
		t.Fatal(err)
	}
	s.headers = response.Header
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.Header.Get("CSeq") != fmt.Sprint(s.seq) {
		t.Fatal("CSeq lost")
	}
	return response.StatusCode, data
}

func (s *testSender) pair(t *testing.T) []byte {
	return s.pairWithSetup(t, true)
}

func (s *testSender) pairWithSetup(t *testing.T, setup bool) []byte {
	t.Helper()
	_, signing, _ := ed25519.GenerateKey(rand.Reader)
	key, _ := ecdh.X25519().GenerateKey(rand.Reader)
	var status int
	var receiverSigning []byte
	if setup {
		status, receiverSigning = s.exchange(t, "POST", "/pair-setup", signing.Public().(ed25519.PublicKey))
	} else {
		var data []byte
		status, data = s.exchange(t, "GET", "/info", nil)
		var info struct {
			PK []byte `plist:"pk"`
		}
		if _, err := plist.Unmarshal(data, &info); err != nil {
			t.Fatal(err)
		}
		receiverSigning = info.PK
	}
	if status != 200 || len(receiverSigning) != 32 {
		t.Fatal(status)
	}
	first := append([]byte{1, 0, 0, 0}, key.PublicKey().Bytes()...)
	first = append(first, signing.Public().(ed25519.PublicKey)...)
	status, response := s.exchange(t, "POST", "/pair-verify", first)
	if status != 200 || len(response) != 96 {
		t.Fatal(status)
	}
	peer, _ := ecdh.X25519().NewPublicKey(response[:32])
	secret, err := key.ECDH(peer)
	if err != nil {
		t.Fatal(err)
	}
	derive := func(label string) []byte { sum := sha512.Sum512(append([]byte(label), secret...)); return sum[:16] }
	block, _ := aes.NewCipher(derive("Pair-Verify-AES-Key"))
	stream := cipher.NewCTR(block, derive("Pair-Verify-AES-IV"))
	sig := make([]byte, 64)
	stream.XORKeyStream(sig, response[32:])
	if !ed25519.Verify(receiverSigning, append(append([]byte(nil), response[:32]...), key.PublicKey().Bytes()...), sig) {
		t.Fatal("receiver signature")
	}
	sig = ed25519.Sign(signing, append(append([]byte(nil), key.PublicKey().Bytes()...), response[:32]...))
	stream.XORKeyStream(sig, sig)
	if status, _ := s.exchange(t, "POST", "/pair-verify", append([]byte{0, 0, 0, 0}, sig...)); status != 200 {
		t.Fatal(status)
	}
	return secret
}

func (s *testSender) fairplay(t *testing.T) []byte {
	t.Helper()
	first := make([]byte, 16)
	copy(first, "FPLY")
	first[4], first[5], first[6], first[14] = 3, 1, 1, 2
	binary.BigEndian.PutUint32(first[8:], 4)
	status, reply := s.exchange(t, "POST", "/fp-setup", first)
	if status != 200 || len(reply) != 142 || reply[13] != 2 {
		t.Fatal(status, len(reply))
	}
	message := make([]byte, 164)
	for i := range message {
		message[i] = byte(i)
	}
	copy(message, "FPLY")
	message[4], message[5], message[6], message[7], message[12] = 3, 1, 3, 0, 2
	binary.BigEndian.PutUint32(message[8:], 152)
	status, reply = s.exchange(t, "POST", "/fp-setup", message)
	if status != 200 || len(reply) != 32 || !bytes.Equal(reply[12:], message[144:]) {
		t.Fatal(status)
	}
	return message
}

func TestReceiverNativeHandshakeEncryptedVideoAndRestart(t *testing.T) {
	if err := playfair.Available(); err != nil {
		t.Skip(err)
	}
	r, err := NewReceiver("airplay:AnyKVM-Test", Options{})
	if err != nil {
		t.Fatal(err)
	}
	r.browserMedia = true
	start := func() string {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		r.start(listener, nil)
		return listener.Addr().String()
	}
	address := start()
	t.Cleanup(func() { r.Close() })
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sender := &testSender{conn: conn, reader: bufio.NewReader(conn)}
	status, info := sender.exchange(t, "GET", "/info", nil)
	var capabilities map[string]any
	if _, err := plist.Unmarshal(info, &capabilities); err != nil || status != 200 || capabilities["name"] != "AnyKVM-Test" {
		t.Fatal(capabilities, status, err)
	}
	qualifier, _ := plist.Marshal(map[string]any{"qualifier": []string{"txtAirPlay", "txtRAOP"}}, plist.BinaryFormat)
	status, info = sender.exchange(t, "GET", "/info", qualifier)
	var selected map[string][]byte
	if _, err := plist.Unmarshal(info, &selected); err != nil || status != 200 || len(selected) != 2 || !bytes.Equal(selected["txtAirPlay"], encodeTXT(r.textRecords())) || !bytes.Equal(selected["txtRAOP"], encodeTXT(r.raopRecords())) {
		t.Fatal("qualified discovery info changed", selected, status, err)
	}
	if status, _ := sender.exchange(t, "SETUP", "/stream", nil); status != 403 {
		t.Fatal("unauthenticated setup", status)
	}
	// That 403 closes the connection. Start a new authenticated sender.
	conn.Close()
	conn, err = net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sender = &testSender{conn: conn, reader: bufio.NewReader(conn)}
	secret := sender.pair(t)
	message := sender.fairplay(t)
	timing, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer timing.Close()
	ek := make([]byte, 72)
	for i := range ek {
		ek[i] = byte(255 - i)
	}
	body, err := plist.Marshal(map[string]any{"ekey": ek, "eiv": make([]byte, 16), "timingPort": uint64(timing.LocalAddr().(*net.UDPAddr).Port)}, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	status, setupBody := sender.exchange(t, "SETUP", "/stream", body)
	if status != 200 {
		t.Fatal("key setup", status)
	}
	var transport map[string]uint64
	if _, err := plist.Unmarshal(setupBody, &transport); err != nil || transport["timingPort"] == 0 {
		t.Fatal("missing RTP timing transport", transport, err)
	}
	if eventPort, ok := transport["eventPort"]; !ok || eventPort != 0 {
		t.Fatal("advertised an unimplemented RTP event channel", transport)
	}
	if status, _ := sender.exchange(t, "RECORD", "/stream", nil); status != 200 || sender.headers.Get("Audio-Latency") != "0" || sender.headers.Get("Audio-Jack-Status") != "connected; type=analog" {
		t.Fatal("RTP RECORD output not ready", status, sender.headers)
	}
	body, _ = plist.Marshal(map[string]any{"streams": []any{map[string]any{"type": uint64(96)}, map[string]any{"type": uint64(110), "streamConnectionID": uint64(123)}}}, plist.BinaryFormat)
	status, response := sender.exchange(t, "SETUP", "/stream", body)
	var setup struct {
		Streams []struct {
			Type        uint64 `plist:"type"`
			DataPort    uint64 `plist:"dataPort"`
			ControlPort uint64 `plist:"controlPort"`
		} `plist:"streams"`
	}
	if _, err := plist.Unmarshal(response, &setup); err != nil || status != 200 || len(setup.Streams) != 2 || setup.Streams[0].Type != 96 || setup.Streams[1].Type != 110 {
		t.Fatal(status, err)
	}
	// Companion RTP/control packets are drained silently, and their workers
	// must close together with video so a Mirror session cannot hang teardown.
	for _, port := range []uint64{setup.Streams[0].DataPort, setup.Streams[0].ControlPort} {
		if port == 0 {
			t.Fatal("audio sink missing port")
		}
		audio, err := net.Dial("udp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := audio.Write([]byte{0x80, 0x60, 0, 1}); err != nil {
			t.Fatal(err)
		}
		audio.Close()
	}
	video, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", setup.Streams[1].DataPort))
	if err != nil {
		t.Fatal(err)
	}
	defer video.Close()
	config, idr, want := fixtureAVC(t)
	key, err := playfair.Decrypt(message, ek)
	if err != nil {
		t.Fatal(err)
	}
	encrypt, err := mirrorCipher(key, secret, 123)
	if err != nil {
		t.Fatal(err)
	}
	encoded := binary.BigEndian.AppendUint32(nil, uint32(len(idr)))
	encoded = append(encoded, idr...)
	encrypt.XORKeyStream(encoded, encoded)
	// Write a fragmented wire buffer to exercise io.ReadFull framing.
	wire := append(mirrorWire(1, config), mirrorWire(2, nil)...)
	wire = append(wire, mirrorWire(0, encoded)...)
	video.SetWriteDeadline(time.Now().Add(3 * time.Second))
	for len(wire) > 0 {
		n := min(17, len(wire))
		if _, err := video.Write(wire[:n]); err != nil {
			t.Fatal(err)
		}
		wire = wire[n:]
	}
	var frame *capture.Frame
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && frame == nil; {
		frame, err = r.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
	}
	if frame == nil || frame.Format != capture.FormatH264 || frame.Width != 64 || frame.Height != 48 || !bytes.Equal(frame.Data, want) {
		t.Fatal("wrong captured frame", frame)
	}
	// YouTube can stop companion audio without stopping screen mirroring.
	// Exercise the real control connection and continuous video cipher across
	// audio teardown/re-SETUP, rather than only testing the request handler.
	teardown := func(types ...uint64) []byte {
		streams := make([]map[string]any, 0, len(types))
		for _, kind := range types {
			streams = append(streams, map[string]any{"type": kind})
		}
		body, err := plist.Marshal(map[string]any{"streams": streams}, plist.BinaryFormat)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	r.mu.Lock()
	media := r.owner.media
	r.mu.Unlock()
	audioSockets := func() (*net.UDPConn, *net.UDPConn) {
		media.mu.Lock()
		defer media.mu.Unlock()
		return media.audioData, media.audioControl
	}
	writePicture := func() {
		t.Helper()
		encoded := binary.BigEndian.AppendUint32(nil, uint32(len(idr)))
		encoded = append(encoded, idr...)
		encrypt.XORKeyStream(encoded, encoded)
		video.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if _, err := video.Write(mirrorWire(0, encoded)); err != nil {
			t.Fatal(err)
		}
	}
	checkPicture := func() {
		t.Helper()
		var frame *capture.Frame
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && frame == nil; {
			frame, err = r.ReadFrame()
			if err != nil {
				t.Fatal(err)
			}
		}
		if frame == nil || !bytes.Equal(frame.Data, want) {
			t.Fatal("video lost across stream teardown", frame)
		}
	}
	for _, invalid := range []struct {
		body   []byte
		status int
	}{{[]byte("invalid plist"), 400}, {teardown(96, 999), 461}, {teardown(96, 96), 461}} {
		oldData, oldControl := audioSockets()
		if status, _ := sender.exchange(t, "TEARDOWN", "/stream", invalid.body); status != invalid.status {
			t.Fatal("invalid teardown", status)
		}
		if data, control := audioSockets(); data != oldData || control != oldControl {
			t.Fatal("invalid teardown modified audio")
		}
		writePicture()
		checkPicture()
	}
	for range 2 {
		data, control := audioSockets()
		if status, _ := sender.exchange(t, "TEARDOWN", "/stream", teardown(96)); status != 200 {
			t.Fatal("audio teardown", status)
		}
		for _, socket := range []*net.UDPConn{data, control} {
			if err := socket.SetReadDeadline(time.Now()); !errors.Is(err, net.ErrClosed) {
				t.Fatal("audio socket left open", err)
			}
		}
		if status, _ := sender.exchange(t, "POST", "/feedback", nil); status != 200 {
			t.Fatal("control lost after audio teardown", status)
		}
		writePicture()
		checkPicture()
		// Repeated teardown must be harmless, then audio can be set up again
		// with the existing session keys and timing connection.
		if status, _ := sender.exchange(t, "TEARDOWN", "/stream", teardown(96)); status != 200 {
			t.Fatal(status)
		}
		status, response = sender.exchange(t, "SETUP", "/stream", teardown(96))
		setup.Streams = nil
		if _, err := plist.Unmarshal(response, &setup); err != nil || status != 200 || len(setup.Streams) != 1 || setup.Streams[0].DataPort == 0 || setup.Streams[0].ControlPort == 0 {
			t.Fatal("audio restart", status, err)
		}
		writePicture()
		checkPicture()
	}
	// Stopping video must release a receiver blocked behind a full frame
	// queue, without terminating the companion audio or control connection.
	for range 4 {
		writePicture()
	}
	data, control := audioSockets()
	if status, _ := sender.exchange(t, "TEARDOWN", "/stream", teardown(110)); status != 200 {
		t.Fatal("video teardown", status)
	}
	if nextData, nextControl := audioSockets(); data != nextData || control != nextControl {
		t.Fatal("video teardown stopped audio")
	}
	if err := data.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal("audio closed by video teardown", err)
	}
	body, _ = plist.Marshal(map[string]any{"streams": []any{map[string]any{"type": uint64(110), "streamConnectionID": uint64(124)}}}, plist.BinaryFormat)
	status, response = sender.exchange(t, "SETUP", "/stream", body)
	setup.Streams = nil
	if _, err := plist.Unmarshal(response, &setup); err != nil || status != 200 || len(setup.Streams) != 1 {
		t.Fatal("video restart", status, err)
	}
	video.Close()
	video, err = net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", setup.Streams[0].DataPort))
	if err != nil {
		t.Fatal(err)
	}
	defer video.Close()
	encrypt, err = mirrorCipher(key, secret, 124)
	if err != nil {
		t.Fatal(err)
	}
	video.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := video.Write(mirrorWire(1, config)); err != nil {
		t.Fatal(err)
	}
	writePicture()
	checkPicture()
	if status, _ := sender.exchange(t, "TEARDOWN", "/stream", teardown(96, 110)); status != 200 {
		t.Fatal("both stream teardown", status)
	}
	// A sender can retain pairing/timing while switching from RTP to HTTP
	// video. Stopped streams must not reserve the receiver indefinitely.
	startFCUPSender(t, "http://"+address, "transition", func(q fcupTestRequest) error {
		if q.URL != "mlhls://localhost/master.m3u8" {
			return fmt.Errorf("unexpected transition resource")
		}
		code, err := fcupAction("http://"+address, "transition", q, 200, []byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000,CODECS=\"avc1.64001f,mp4a.40.2\"\nvideo.m3u8\n"))
		if err == nil && code != 200 {
			return fmt.Errorf("transition FCUP reply %d", code)
		}
		return err
	})
	if status, _ := mediaControl(t, "http://"+address, "POST", "/play", "transition", []byte("Content-Location: mlhls://localhost/master.m3u8")); status != 200 {
		t.Fatal("HTTP playback after stream teardown", status)
	}
	device := &Device{receiver: r}
	state := device.BrowserMediaState()
	if state == nil {
		t.Fatal("transition did not publish Web media")
	}
	playlist := httptest.NewRecorder()
	device.ServeBrowserMedia(playlist, httptest.NewRequest("GET", state.URL, nil))
	if playlist.Code != 200 || !strings.Contains(playlist.Body.String(), "/airplay/media/"+state.ID+"/") {
		t.Fatal("transition FCUP playlist unavailable", playlist.Code)
	}
	if status, _ := sender.exchange(t, "SETUP", "/stream", body); status != 453 {
		t.Fatal("old pairing displaced HTTP playback", status)
	}
	if status, _ := mediaControl(t, "http://"+address, "POST", "/stop", "transition", nil); status != 200 {
		t.Fatal("HTTP playback stop", status)
	}
	// Reuse the existing keys after URL playback ends, then verify that the
	// restarted mirror releases ownership on another stream-specific stop.
	if status, _ := sender.exchange(t, "SETUP", "/stream", body); status != 200 {
		t.Fatal("mirror restart after HTTP playback", status)
	}
	if status, _ := sender.exchange(t, "TEARDOWN", "/stream", teardown(110)); status != 200 {
		t.Fatal("restarted mirror teardown", status)
	}
	r.mu.Lock()
	owned := r.owner != nil
	r.mu.Unlock()
	if owned {
		t.Fatal("stopped mirror retained ownership")
	}
	if status, _ := sender.exchange(t, "POST", "/feedback", nil); status != 200 {
		t.Fatal("stream teardown lost pairing", status)
	}
	if status, _ := sender.exchange(t, "TEARDOWN", "/stream", nil); status != 200 {
		t.Fatal(status)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := sender.reader.ReadByte(); err != io.EOF {
		t.Fatal("full teardown did not close control", err)
	}
	closed := make(chan struct{})
	go func() { r.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("receiver close blocked")
	}
	if _, err := r.ReadFrame(); err != capture.ErrStreamStopped {
		t.Fatal(err)
	}
	// The same receiver can reopen after all old workers have stopped.
	address = start()
	conn2, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	second := &testSender{conn: conn2, reader: bufio.NewReader(conn2)}
	if status, _ := second.exchange(t, "OPTIONS", "*", nil); status != 200 {
		t.Fatal(status)
	}
	// iOS reconnects with pair-verify using the public key advertised by Bonjour.
	second.pairWithSetup(t, false)
}

func TestOptionsRejectInvalidSourceAndAddress(t *testing.T) {
	for _, s := range []string{"airplay:", "airplay:\nfoo", "ios-airplay", "airplay:" + strings.Repeat("a", 64)} {
		if _, err := ReceiverName(s); err == nil {
			t.Fatal(s)
		}
	}
	for _, addr := range []string{"localhost:7000", ":0", ":65536", "7000"} {
		if err := (Options{Addr: addr}).Validate(); err == nil {
			t.Fatal(addr)
		}
	}
	if err := (Options{Addr: "[::1]:7000"}).Validate(); err != nil {
		t.Fatal(err)
	}
}
