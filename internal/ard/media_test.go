package ard

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"anykvm/internal/avmedia"
)

func TestMediaOfferIndependentPlistDecode(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable for independent binary-plist validation")
	}
	for _, audio := range []bool{false, true} {
		p, err := mediaOffer(audio, 0x11223344)
		if err != nil {
			t.Fatal(err)
		}
		mode := "7"
		if audio {
			mode = "8"
		}
		cmd := exec.Command(python, "-c", `import sys,plistlib,zlib,uuid
p=plistlib.loads(sys.stdin.buffer.read())
assert p['avcMediaStreamNegotiatorMode']==int(sys.argv[1])
uuid.UUID(p['avcMediaStreamOptionCallID'])
b=zlib.decompress(p['avcMediaStreamNegotiatorMediaBlob'])
assert b'Viceroy 1.7.0' in b
assert len(p['avcMediaStreamOptionRemoteEndpointInfo'])>0
`, mode)
		cmd.Stdin = bytes.NewReader(p)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("independent plist/zlib decode: %v %s", err, output)
		}
	}
}

func annexBNALs(p []byte) [][]byte {
	var nals [][]byte
	start := -1
	for i := 0; i+3 <= len(p); i++ {
		prefix := 0
		if bytes.Equal(p[i:i+3], []byte{0, 0, 1}) {
			prefix = 3
		} else if i+4 <= len(p) && bytes.Equal(p[i:i+4], []byte{0, 0, 0, 1}) {
			prefix = 4
		}
		if prefix != 0 {
			if start >= 0 {
				nals = append(nals, p[start:i])
			}
			start = i + prefix
			i += prefix - 1
		}
	}
	if start >= 0 {
		nals = append(nals, p[start:])
	}
	return nals
}
func protectRTP(c *mediaCrypto, p []byte, roc uint32) []byte {
	h, _ := rtpHeaderLen(p)
	out := bytes.Clone(p)
	c.transform(out[h:], be32(p[8:]), uint64(roc)<<16|uint64(be16(p[2:])))
	mac := hmac.New(sha1.New, c.auth)
	mac.Write(out)
	mac.Write(binary.BigEndian.AppendUint32(nil, roc))
	return append(out, mac.Sum(nil)[:10]...)
}

func TestSRTPSequenceWrapAndOutOfOrderReplay(t *testing.T) {
	key := make([]byte, 46)
	for i := range key {
		key[i] = byte(i)
	}
	crypto, _ := newMediaCrypto(key, 0)
	receiver := &srtpReceiver{mediaCrypto: crypto}
	for _, v := range []struct {
		seq uint16
		roc uint32
	}{{65534, 0}, {0, 1}, {65535, 0}, {1, 1}} {
		p := rtpPacket(v.seq, 1, true, []byte{38, 1, 42})
		encrypted := protectRTP(crypto, p, v.roc)
		out, err := receiver.unprotect(encrypted)
		if err != nil || !bytes.Equal(out, p) {
			t.Fatalf("wrap sequence %d: %v", v.seq, err)
		}
		if _, err := receiver.unprotect(encrypted); err == nil {
			t.Fatal("accepted reordered replay")
		}
	}
}

func TestHEVCSPSFixture(t *testing.T) {
	p, err := os.ReadFile("../testdata/passthrough.hevc")
	if err != nil {
		t.Fatal(err)
	}
	for _, nal := range annexBNALs(p) {
		if nal[0]>>1&63 == 33 {
			info, err := parseSPS(nal)
			if err != nil || info.Width != 64 || info.Height != 48 || info.Chroma != 1 || info.DepthLuma != 8 {
				t.Fatalf("SPS=%+v err=%v", info, err)
			}
			return
		}
	}
	t.Fatal("no SPS fixture")
}

func readControlFixture(conn net.Conn, c *controlCipher) ([]byte, error) {
	h, err := readExactly(conn, 2)
	if err != nil {
		return nil, err
	}
	p, err := readExactly(conn, int(be16(h)))
	if err != nil {
		return nil, err
	}
	return c.decrypt(p)
}
func writeControlFixture(conn net.Conn, c *controlCipher, p []byte) error {
	p, err := c.encrypt(p)
	if err != nil {
		return err
	}
	_, err = conn.Write(append([]byte{byte(len(p) >> 8), byte(len(p))}, p...))
	return err
}
func updateFixture(encoding int32, payload []byte) []byte {
	p := make([]byte, 16)
	p[0] = 0
	put16(p[2:], 1)
	put32(p[12:], uint32(encoding))
	return append(p, payload...)
}

// Uses a synthetic peer, real TCP/UDP, cipher suite 5, FU packets, real libav
// HEVC decode and the Capture lifecycle. It does not establish Mac compatibility.
func TestHEVCEncryptedCaptureRoundTrip(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_AVMEDIA") == "" {
		t.Skip("set ANYKVM_TEST_AVMEDIA and ANYKVM_FFMPEG_LIBDIR")
	}
	source, err := avmedia.OpenVideoSource("mov", "../testdata/passthrough_hevc.mp4", nil, "hevc")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	var pictures [][]byte
	for {
		f, err := source.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if f != nil {
			pictures = append(pictures, bytes.Clone(f.Data))
		}
	}
	if len(pictures) != 4 {
		t.Fatalf("expected 4 fixture pictures, got %d", len(pictures))
	}
	decoder, err := avmedia.NewDecoder("hevc")
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	var expected [][]byte
	for _, p := range pictures {
		f, err := decoder.Decode(p)
		if err != nil || f == nil {
			t.Fatal("independent fixture decode", err)
		}
		expected = append(expected, bytes.Clone(f.Data))
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	lc := net.ListenConfig{Control: reuseMediaPort}
	packetConn, err := lc.ListenPacket(context.Background(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udp := packetConn.(*net.UDPConn)
	defer udp.Close()
	serverErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		serverErr <- serveHEVCFixture(conn, udp, key, pictures)
	}()
	client, err := NewClient("ard://"+ln.Addr().String(), Options{Username: "tester", Password: "test-password", Codec: "hevc", Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Open(); err != nil {
		select {
		case peerErr := <-serverErr:
			t.Fatalf("open: %v; peer: %v", err, peerErr)
		default:
			t.Fatal(err)
		}
	}
	var previousTime time.Time
	for i := 0; i < 4; i++ {
		var pixels []byte
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			f, err := client.Capture()
			if err != nil {
				t.Fatal(err)
			}
			if f == nil {
				continue
			}
			if f.Width != 64 || f.Height != 48 || f.CapturedAt.IsZero() {
				t.Fatal("missing source metadata")
			}
			if !previousTime.IsZero() && !f.CapturedAt.After(previousTime) {
				t.Fatal("stale presentation timestamp")
			}
			previousTime = f.CapturedAt
			pixels = f.Data
			break
		}
		if len(pixels) != 64*48*4 {
			t.Fatalf("picture %d not decoded", i)
		}
		if !bytes.Equal(expected[i], pixels) {
			t.Fatalf("picture %d differs from independent decode", i)
		}
	}
	if err := client.KeyDown(0x04); err != nil {
		t.Fatal(err)
	}
	if err := client.MouseButton(1, true); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-serverErr; err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

func serveHEVCFixture(conn net.Conn, udp *net.UDPConn, key *rsa.PrivateKey, pictures [][]byte) error {
	var sessionKey []byte
	if err := serveMacAuthSized(conn, key, "RFB 003.889\n", false, 0, 0, &sessionKey); err != nil {
		return err
	}
	if _, err := readExactly(conn, 20); err != nil {
		return err
	}
	enc, err := readExactly(conn, 4)
	if err != nil {
		return err
	}
	if _, err := readExactly(conn, int(be16(enc[2:]))*4); err != nil {
		return err
	}
	if _, err := readExactly(conn, 66+16+10); err != nil {
		return err
	}
	controlKey, iv := bytes.Repeat([]byte{7}, 16), bytes.Repeat([]byte{9}, 16)
	block, _ := aes.NewCipher(sessionKey)
	offer := make([]byte, 36)
	put32(offer, 1)
	block.Encrypt(offer[4:20], controlKey)
	block.Encrypt(offer[20:36], iv)
	if _, err := conn.Write(updateFixture(1103, offer)); err != nil {
		return err
	}
	if _, err := readExactly(conn, 8); err != nil {
		return err
	}
	cipher, err := newControlCipher(controlKey, iv)
	if err != nil {
		return err
	}
	for i := 0; i < 2; i++ {
		if _, err := readControlFixture(conn, cipher); err != nil {
			return err
		}
	}
	body := make([]byte, 36)
	put16(body, 1)
	put16(body[2:], 1)
	port := udp.LocalAddr().(*net.UDPAddr).Port
	put16(body[14:], port)
	if err := writeControlFixture(conn, cipher, updateFixture(1010, append([]byte{0, 36}, body...))); err != nil {
		return err
	}
	config, err := readControlFixture(conn, cipher)
	if err != nil {
		return err
	}
	if len(config) < 220 || config[0] != 28 || int(be16(config[2:])) != len(config)-4 {
		return fmt.Errorf("invalid media configuration")
	}
	audioLen, videoLen := int(be16(config[10:])), int(be16(config[12:]))
	if len(config) != 220+audioLen+videoLen {
		return fmt.Errorf("bad compound offer lengths")
	}
	if !bytes.HasPrefix(config[128:128+audioLen], []byte("bplist00")) || !bytes.HasPrefix(config[220+audioLen:], []byte("bplist00")) {
		return fmt.Errorf("invalid media offer plist")
	}
	crypto, err := newMediaCrypto(config[174+audioLen:220+audioLen], 0)
	if err != nil {
		return err
	}
	peer := conn.RemoteAddr().(*net.TCPAddr)
	dst := &net.UDPAddr{IP: peer.IP, Port: port}
	seq := uint16(1000)
	for i, picture := range pictures {
		nals := annexBNALs(picture)
		var packets [][]byte
		for j, nal := range nals {
			if len(nal) < 2 {
				return fmt.Errorf("truncated fixture NAL")
			}
			// Force FU reception even for small synthetic slices.
			if len(nal) > 100 {
				for off := 2; off < len(nal); {
					n := min(100, len(nal)-off)
					fu := []byte{nal[0]&129 | 98, nal[1], (nal[0] >> 1) & 63}
					if off == 2 {
						fu[2] |= 128
					}
					if off+n == len(nal) {
						fu[2] |= 64
					}
					p := rtpPacket(seq, uint32(90000+i*3000), j == len(nals)-1 && off+n == len(nal), append(fu, nal[off:off+n]...))
					packets = append(packets, protectRTP(crypto, p, 0))
					seq++
					off += n
				}
			} else {
				p := rtpPacket(seq, uint32(90000+i*3000), j == len(nals)-1, nal)
				packets = append(packets, protectRTP(crypto, p, 0))
				seq++
			}
		}
		// Start with a stable packet, then reverse one adjacent pair.
		if len(packets) > 4 {
			packets[2], packets[3] = packets[3], packets[2]
		}
		for _, p := range packets {
			if _, err := udp.WriteToUDP(p, dst); err != nil {
				return err
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	for {
		p, err := readControlFixture(conn, cipher)
		if err != nil {
			return err
		}
		if len(p) == 0 {
			return fmt.Errorf("empty input")
		}
	}
}
