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
	"encoding/hex"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestControlFramingAndLimits(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader("POST /pair-setup RTSP/1.0\r\nCSeq: 3\r\nContent-Length: 3\r\n\r\nabcGET /info HTTP/1.1\r\n\r\n"), maxControlHeaders)
	q, err := readRequest(r)
	if err != nil || q.method != "POST" || q.path != "/pair-setup" || string(q.body) != "abc" {
		t.Fatal(q, err)
	}
	var response bytes.Buffer
	if err := writeResponse(&response, q, 200, []byte("xyz"), "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(response.String(), "RTSP/1.0 200 OK\r\n") || !strings.Contains(response.String(), "CSeq: 3\r\n") {
		t.Fatal(response.String())
	}
	if q, err := readRequest(r); err != nil || q.path != "/info" {
		t.Fatal(q, err)
	}
	for _, wire := range []string{
		"GET /info HTTP/2\r\n\r\n",
		"POST /x RTSP/1.0\r\nContent-Length: -1\r\n\r\n",
		"POST /x RTSP/1.0\r\nContent-Length: 65537\r\n\r\n",
		"POST /x RTSP/1.0\r\nContent-Length: 1\r\nContent-Length: 1\r\n\r\na",
		"GET /x RTSP/1.0\r\nCSeq: 1\r\nCSeq: 2\r\n\r\n",
		"POST /x RTSP/1.0\r\nTransfer-Encoding: chunked\r\n\r\n",
		"POST /x RTSP/1.0\r\nContent-Length: 2\r\n\r\na",
		"GET /x RTSP/1.0\r\nX: " + strings.Repeat("a", maxControlHeaders) + "\r\n\r\n",
	} {
		if _, err := readRequest(bufio.NewReaderSize(strings.NewReader(wire), maxControlHeaders)); err == nil {
			t.Fatalf("accepted malformed request %q", wire[:min(80, len(wire))])
		}
	}
}

func TestAirPlayRTSPIPv6SessionTargets(t *testing.T) {
	for _, target := range []string{
		"rtsp://fe80::1234:5678:9abc:def0/1234567890123456789",
		"rtsp://fe80::1234:5678:9abc:def0%en0/1234567890123456789",
		"rtsp://[fe80::1234:5678:9abc:def0]/1234567890123456789",
		"rtsp://[fe80::1234:5678:9abc:def0]:7100/1234567890123456789",
		"rtsp://192.0.2.10:7100/1234567890123456789",
	} {
		wire := "SETUP " + target + " RTSP/1.0\r\nCSeq: 5\r\nContent-Length: 0\r\n\r\n"
		q, err := readRequest(bufio.NewReaderSize(strings.NewReader(wire), maxControlHeaders))
		if err != nil || q.method != "SETUP" || q.path != "/1234567890123456789" {
			t.Fatal(target, q, err)
		}
	}
	if _, err := readRequest(bufio.NewReaderSize(strings.NewReader("SETUP rtsp://not::ipv6/session RTSP/1.0\r\n\r\n"), maxControlHeaders)); err == nil {
		t.Fatal("invalid IPv6 authority accepted")
	}
}

func TestLegacyPairingProvesBothSignaturesAndRejectsReplay(t *testing.T) {
	_, receiver, _ := ed25519.GenerateKey(rand.Reader)
	p := pairing{private: receiver}
	public, err := p.pairSetup(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	peerKey, _ := ecdh.X25519().GenerateKey(rand.Reader)
	peerPublic, peerSigning, _ := ed25519.GenerateKey(rand.Reader)
	first := append([]byte{1, 0, 0, 0}, peerKey.PublicKey().Bytes()...)
	first = append(first, peerPublic...)
	response, err := p.pairVerify(first)
	if err != nil {
		t.Fatal(err)
	}
	remote, _ := ecdh.X25519().NewPublicKey(response[:32])
	secret, err := peerKey.ECDH(remote)
	if err != nil {
		t.Fatal(err)
	}
	derive := func(label string) []byte { sum := sha512.Sum512(append([]byte(label), secret...)); return sum[:16] }
	block, _ := aes.NewCipher(derive("Pair-Verify-AES-Key"))
	stream := cipher.NewCTR(block, derive("Pair-Verify-AES-IV"))
	sig := make([]byte, 64)
	stream.XORKeyStream(sig, response[32:])
	if !ed25519.Verify(public, append(append([]byte(nil), response[:32]...), peerKey.PublicKey().Bytes()...), sig) {
		t.Fatal("receiver signature invalid")
	}
	sig = ed25519.Sign(peerSigning, append(append([]byte(nil), peerKey.PublicKey().Bytes()...), response[:32]...))
	stream.XORKeyStream(sig, sig)
	final := append([]byte{0, 0, 0, 0}, sig...)
	if _, err := p.pairVerify(final); err != nil || !p.verified || !bytes.Equal(p.secret, secret) {
		t.Fatal("sender signature failed", err)
	}
	if _, err := p.pairVerify(final); err == nil {
		t.Fatal("replayed final message accepted")
	}
	p = pairing{private: receiver}
	if _, err := p.pairVerify(first); err != nil || p.verified {
		t.Fatal("verify using advertised public key must start without authenticating yet", err)
	}
	p.pairSetup(make([]byte, 32))
	if _, err := p.pairVerify(append([]byte{1, 0, 0, 0}, make([]byte, 64)...)); err == nil {
		t.Fatal("low-order X25519 key accepted")
	}
}

func fixtureAVC(t *testing.T) (config, idr, annexb []byte) {
	t.Helper()
	data, err := os.ReadFile("../testdata/passthrough.h264")
	if err != nil {
		t.Fatal(err)
	}
	var nals [][]byte
	for len(data) > 0 {
		start := bytes.Index(data, []byte{0, 0, 1})
		if start < 0 {
			break
		}
		data = data[start+3:]
		end := bytes.Index(data, []byte{0, 0, 1})
		if end < 0 {
			nals = append(nals, data)
			break
		}
		nal := data[:end]
		if len(nal) > 0 && nal[len(nal)-1] == 0 {
			nal = nal[:len(nal)-1]
		}
		nals = append(nals, nal)
		data = data[end:]
	}
	var sps, pps []byte
	for _, nal := range nals {
		if len(nal) == 0 {
			continue
		}
		switch nal[0] & 31 {
		case 7:
			sps = nal
		case 8:
			pps = nal
		case 5:
			idr = nal
		}
	}
	if len(sps) == 0 || len(pps) == 0 || len(idr) == 0 {
		t.Fatal("incomplete H.264 fixture")
	}
	config = []byte{1, sps[1], sps[2], sps[3], 0xff, 0xe1}
	config = binary.BigEndian.AppendUint16(config, uint16(len(sps)))
	config = append(config, sps...)
	config = append(config, 1)
	config = binary.BigEndian.AppendUint16(config, uint16(len(pps)))
	config = append(config, pps...)
	for _, nal := range [][]byte{sps, pps, idr} {
		annexb = append(annexb, 0, 0, 0, 1)
		annexb = append(annexb, nal...)
	}
	return
}

func mirrorWire(kind uint16, payload []byte) []byte {
	header := make([]byte, 128)
	binary.LittleEndian.PutUint32(header, uint32(len(payload)))
	binary.LittleEndian.PutUint16(header[4:], kind)
	return append(header, payload...)
}

func TestMirrorPacketsEncryptedContinuityAndRotation(t *testing.T) {
	config, idr, want := fixtureAVC(t)
	block, _ := aes.NewCipher(make([]byte, 16))
	encrypt := cipher.NewCTR(block, make([]byte, 16))
	d := NewMirrorDecoder(cipher.NewCTR(block, make([]byte, 16)))
	decode := func(kind uint16, data []byte) []byte {
		t.Helper()
		packet, err := ReadMirrorPacket(bytes.NewReader(mirrorWire(kind, data)))
		if err != nil {
			t.Fatal(err)
		}
		au, err := d.Decode(packet)
		if err != nil {
			t.Fatal(err)
		}
		return au
	}
	decode(1, config)
	decode(2, nil)
	if d.width != 64 || d.height != 48 {
		t.Fatal(d.width, d.height)
	}
	video := binary.BigEndian.AppendUint32(nil, uint32(len(idr)))
	video = append(video, idr...)
	for i := 0; i < 2; i++ {
		ciphertext := append([]byte(nil), video...)
		encrypt.XORKeyStream(ciphertext, ciphertext)
		if au := decode(0, ciphertext); !bytes.Equal(au, want) {
			t.Fatal("changed IDR or cipher reset", i)
		}
		decode(2, nil)
		// Reports can occur between encrypted pictures and must not advance
		// the cipher or emit video, even with a non-block-sized payload.
		if au := decode(5, []byte("streaming report payload")); au != nil {
			t.Fatal("streaming report emitted video")
		}
	}
	// A lock/wake configuration repeat must keep references and cipher state.
	decode(1, config)
	delta := []byte{0, 0, 0, 2, 0x41, 0x80}
	encrypt.XORKeyStream(delta, delta)
	if au := decode(0, delta); !bytes.Equal(au, []byte{0, 0, 0, 1, 0x41, 0x80}) {
		t.Fatal("repeated configuration blocked resumed delta")
	}
	// A genuinely changed PPS resets recovery, but not the cipher.
	changed := append([]byte(nil), config...)
	changed[len(changed)-1] ^= 1
	decode(1, changed)
	delta = []byte{0, 0, 0, 2, 0x41, 0x80}
	encrypt.XORKeyStream(delta, delta)
	if au := decode(0, delta); au != nil {
		t.Fatal("delta emitted before new IDR")
	}
	// Repeating that new configuration must not bypass the pending IDR.
	decode(1, changed)
	if !d.waitIDR {
		t.Fatal("configuration repeat bypassed recovery")
	}
	video = binary.BigEndian.AppendUint32(nil, uint32(len(idr)))
	video = append(video, idr...)
	encrypt.XORKeyStream(video, video)
	if au := decode(0, video); len(au) == 0 || d.waitIDR {
		t.Fatal("changed configuration failed to recover at IDR")
	}
	for _, wire := range [][]byte{mirrorWire(3, nil), mirrorWire(2, []byte{1}), mirrorWire(0, []byte{1})[:128], make([]byte, 127)} {
		if _, err := ReadMirrorPacket(bytes.NewReader(wire)); err == nil {
			t.Fatal("invalid packet accepted")
		}
	}
	header := make([]byte, 128)
	binary.LittleEndian.PutUint32(header, maxMirrorPayload+1)
	if _, err := ReadMirrorPacket(bytes.NewReader(header)); err == nil {
		t.Fatal("oversized packet accepted")
	}
	if _, _, err := parseAVCConfig(config[:8]); err == nil {
		t.Fatal("truncated codec accepted")
	}
}

func TestMirrorKeyDerivationGolden(t *testing.T) {
	key, secret := make([]byte, 16), make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	for i := range secret {
		secret[i] = byte(i)
	}
	s, err := mirrorCipher(key, secret, ^uint64(0))
	if err != nil {
		t.Fatal(err)
	}
	expectedKey, _ := hex.DecodeString("6bb84d726b8526a4dab0cfdbb7b3b771")
	expectedIV, _ := hex.DecodeString("9d3b8c8a44a0971e889abc4ae83d82a2")
	block, _ := aes.NewCipher(expectedKey)
	expected := cipher.NewCTR(block, expectedIV)
	a, b := make([]byte, 47), make([]byte, 47)
	s.XORKeyStream(a, a)
	expected.XORKeyStream(b, b)
	if !bytes.Equal(a, b) {
		t.Fatal("changed decimal connection ID / SHA512 labels")
	}
}

func TestTimingPresentationUsesMappedSenderUptime(t *testing.T) {
	now := time.Unix(1700000000, 0)
	if micros := ntpMicros(ntpTimestamp(now)) - ntpEpochMicros; micros != now.UnixMicro() {
		t.Fatal(micros)
	}
	timing := timingSession{synced: true, offset: 5000000 - now.UnixMicro()}
	stamp := uint64(5) << 32
	if got := timing.presentationTime(stamp, now); !got.Equal(now) {
		t.Fatal(got)
	}
	if got := timing.presentationTime(0, now); !got.Equal(now) {
		t.Fatal("invalid mapping did not fall back")
	}
}

func FuzzMirrorDecoder(f *testing.F) {
	f.Add([]byte{1, 100, 0, 40, 255, 225, 0, 2, 0x67, 0, 1, 0, 2, 0x68, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 64<<10 {
			t.Skip()
		}
		d := NewMirrorDecoder(nil)
		d.Decode(&MirrorPacket{Type: 1, Payload: b})
		d.Decode(&MirrorPacket{Type: 0, Payload: b})
		_, err := ReadMirrorPacket(io.LimitReader(bytes.NewReader(b), int64(len(b))))
		_ = err
	})
}
