package coredevice

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
	"howett.net/plist"
)

type splitReader struct{ io.Reader }

func (r splitReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.Reader.Read(p)
}

type duplex struct {
	io.Reader
	io.Writer
}

func TestTunnelFragmentedHandshakeAndIPv6(t *testing.T) {
	body := []byte(`{"clientParameters":{"address":"fd00::1","mtu":1280},"serverAddress":"fd00::2","serverRSDPort":58783}`)
	reply := append([]byte("CDTunnel\x00"), byte(len(body)))
	reply = append(reply, body...)
	var written bytes.Buffer
	p, err := handshakeTunnel(duplex{splitReader{bytes.NewReader(reply)}, &written})
	if err != nil || p.ServerAddress != "fd00::2" || p.ServerRSDPort != 58783 {
		t.Fatal(p, err)
	}
	var request map[string]interface{}
	if err = json.Unmarshal(written.Bytes()[10:], &request); err != nil || request["type"] != "clientHandshakeRequest" {
		t.Fatal(request, err)
	}
	// Two packets coalesced in the underlying stream, delivered byte by byte.
	packet := make([]byte, 43)
	packet[0] = 0x60
	packet[5] = 3
	copy(packet[40:], "abc")
	var dst bytes.Buffer
	err = forwardIPv6(&dst, splitReader{bytes.NewReader(append(append([]byte{}, packet...), packet...))})
	if err != io.EOF || !bytes.Equal(dst.Bytes(), append(append([]byte{}, packet...), packet...)) {
		t.Fatal("lost IPv6 boundaries", err)
	}
	for _, bad := range [][]byte{reply[:9], append([]byte("NotATun!\x00"), byte(len(body)))} {
		if _, err := handshakeTunnel(duplex{bytes.NewReader(bad), io.Discard}); err == nil {
			t.Fatal("accepted malformed tunnel")
		}
	}
}
func TestHIDWireReports(t *testing.T) {
	// Fixed wire fixtures: max X, min Y; a six-byte timestamp, not Unix time.
	expected, _ := hex.DecodeString("090105c2ffff00000000000000000000000000000000000000000000000000000000000000000000020000000605040302010000000000000000")
	p := touchReport(true, 32767, -12, 0x010203040506)
	if !bytes.Equal(p, expected) {
		t.Fatalf("touch %x", p)
	}
	p = touchReport(false, 0, 32767, 0)
	if p[3] != 2 || be16(p[6:]) != 65535 {
		t.Fatal("release/coordinate endpoints")
	}
	keys := [30]byte{}
	keys[0] = 0x10
	keys[28] = 2 // A plus left Shift (usage 225).
	p = keyboardReport(keys, 0x010203040506)
	if len(p) != 39 || p[0] != 1 || p[1] != 16 || p[29] != 2 || !bytes.Equal(p[31:37], []byte{6, 5, 4, 3, 2, 1}) {
		t.Fatalf("keyboard %x", p)
	}
	cfg := streamConfig{localSSRC: 1, remoteSSRC: 2}
	feedback := cfg.feedback(0x10001, true)
	if len(feedback) != 56 || feedback[1] != 201 || feedback[33] != 202 || feedback[45] != 206 || binary.BigEndian.Uint32(feedback[16:20]) != 0x10001 || binary.BigEndian.Uint32(feedback[52:56]) != 2 {
		t.Fatalf("RTCP %x", feedback)
	}
}
func TestDisplayOfferAndTrailer(t *testing.T) {
	offer, err := videoOffer(uuid.MustParse("12345678-1234-4234-8234-123456789abc"), 0x11223344)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]interface{}
	if _, err = plist.Unmarshal(offer, &document); err != nil {
		t.Fatal(err)
	}
	if document["avcMediaStreamNegotiatorMode"] != uint64(5) || document["avcMediaStreamOptionCallID"] != "12345678-1234-4234-8234-123456789ABC" {
		t.Fatal(document)
	}
	r, err := zlib.NewReader(bytes.NewReader(document["avcMediaStreamNegotiatorMediaBlob"].([]byte)))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	blob, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(blob, []byte("Viceroy 1.7.0")) || bytes.Contains(blob, []byte("VRAE:0")) {
		t.Fatal("wrong codec profile")
	}
	a := hevcAssembler{waiting: true}
	installSets(&a)
	nal := append([]byte{38, 1, 42}, displayTrailer...)
	au, _, _, err := a.feed(rtpPacket(1, 1, true, nal))
	if err != nil || bytes.Contains(au, displayTrailer) || !bytes.HasSuffix(au, []byte{0, 0, 0, 1, 38, 1, 42}) {
		t.Fatal("display trailer corrupted NAL", err)
	}
}
func TestGoOnlyOptionsAndClosedClient(t *testing.T) {
	for _, field := range []string{"python: python3", "decoder: vt"} {
		var o Options
		d := yaml.NewDecoder(strings.NewReader(field))
		d.KnownFields(true)
		if err := d.Decode(&o); err == nil {
			t.Fatalf("obsolete option accepted: %s", field)
		}
	}
	for _, device := range []string{"ios", "coredevice:", "coredevice:a/b"} {
		if _, err := NewClient(device, Options{}); err == nil {
			t.Fatal("invalid selector", device)
		}
	}
	c, err := NewClient("coredevice", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	if err = c.KeyUp(); err != nil {
		t.Fatal(err)
	}
	if err = c.MouseButton(1, true); err != nil {
		t.Fatal(err)
	}
}
