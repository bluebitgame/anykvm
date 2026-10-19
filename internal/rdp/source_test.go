package rdp

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"anykvm/internal/videodec"
)

func TestSourceNTLMUsesExistingAcceptor(t *testing.T) {
	for _, flags := range []uint32{ntlmRequired, ntlmRequired | ntlmVersion | ntlmKeyExch} {
		negotiate := testNLANegotiate(flags)
		acceptor, e := newNTLMAcceptor(negotiate)
		if e != nil {
			t.Fatal(e)
		}
		auth, peer, e := sourceNTLM(negotiate, acceptor.challenge, "user", "ANYKVM", "secret")
		if e != nil {
			t.Fatal(e)
		}
		server, e := acceptor.authenticate(auth, "user", "secret")
		if e != nil {
			t.Fatal(e)
		}
		p, e := server.unseal(peer.seal([]byte("binding")))
		if e != nil || string(p) != "binding" {
			t.Fatal(e)
		}
		p, e = peer.unseal(server.seal([]byte("server binding")))
		if e != nil || string(p) != "server binding" {
			t.Fatal(e)
		}
		if _, e = acceptor.authenticate(auth, "user", "wrong"); e == nil {
			t.Fatal("wrong password accepted")
		}
	}
}

type sourceRecordingHID struct {
	mu          sync.Mutex
	keys, mouse int
}

func (h *sourceRecordingHID) KeyDown(uint8) error    { h.mu.Lock(); h.keys++; h.mu.Unlock(); return nil }
func (h *sourceRecordingHID) KeyRelease(uint8) error { return nil }
func (h *sourceRecordingHID) KeyUp() error           { return nil }
func (h *sourceRecordingHID) MouseMove(int, int) error {
	h.mu.Lock()
	h.mouse++
	h.mu.Unlock()
	return nil
}
func (h *sourceRecordingHID) MouseMoveRel(int, int) error { return nil }
func (h *sourceRecordingHID) MouseButton(int, bool) error { return nil }
func (h *sourceRecordingHID) MouseWheel(int) error        { return nil }
func sourceFixture(t *testing.T) []byte {
	t.Helper()
	p, e := os.ReadFile("../testdata/passthrough.h264")
	if e != nil {
		t.Fatal(e)
	}
	var out []byte
	for _, n := range videodec.SplitNALs(p) {
		if len(n) > 0 && (n[0]&31 == 7 || n[0]&31 == 8 || n[0]&31 == 5) {
			out = append(out, 0, 0, 0, 1)
			out = append(out, n...)
			if n[0]&31 == 5 {
				break
			}
		}
	}
	return out
}
func TestSourceLoopbackNLAH264InputAndPCM(t *testing.T) {
	decoder, e := videodec.NewH264()
	if e != nil {
		t.Skip("H.264 decoder unavailable", e)
	}
	decoder.Close()
	for _, security := range []string{"nla", "tls"} {
		t.Run(security, func(t *testing.T) {
			h := &sourceRecordingHID{}
			server, e := NewServer(Config{Addr: "127.0.0.1:0", Username: "user", Password: "secret", Codec: "h264", HID: h, DisableClipboard: true, TLSConfig: &tls.Config{Certificates: []tls.Certificate{testNLACertificate(t)}}})
			if e != nil {
				t.Fatal(e)
			}
			if e = server.Start(); e != nil {
				t.Fatal(e)
			}
			defer server.Close()
			dev, _ := NewSource("rdp://user:secret@"+server.listener.Addr().String(), SourceOptions{Timeout: 3 * time.Second, Security: security, InsecureSkipVerify: true})
			c := dev.(*SourceClient)
			if e = c.OpenWithFormat(64, 48, 30); e != nil {
				t.Fatal(e)
			}
			defer c.Close()
			au := sourceFixture(t)
			server.PushFrame(bridge.Frame{Format: capture.FormatH264, Width: 64, Height: 48, Data: au})
			var frame *capture.Frame
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				server.PushFrame(bridge.Frame{Format: capture.FormatH264, Width: 64, Height: 48, Data: au})
				frame, e = c.Capture()
				if e != nil {
					t.Fatal(e)
				}
				if frame != nil {
					break
				}
			}
			if frame == nil || frame.Width != 64 || frame.Height != 48 || frame.Format != capture.FormatRaw || len(frame.Data) != 64*48*4 {
				t.Fatalf("invalid frame %+v", frame)
			}
			if e = c.KeyDown(4); e != nil {
				t.Fatal(e)
			}
			if e = c.KeyRelease(4); e != nil {
				t.Fatal(e)
			}
			if e = c.MouseMove(32767, 32767); e != nil {
				t.Fatal(e)
			}
			audio := c.AudioDevice()
			audio.Open()
			defer audio.Close()
			var af *capture.AudioFrame
			deadline = time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				server.PushAudio(capture.AudioFrame{PCM: bytes.Repeat([]byte{1, 0, 2, 0}, 960), SampleRate: 48000, Channels: 2})
				af, e = audio.Capture()
				if e != nil {
					t.Fatal(e)
				}
				if af != nil {
					break
				}
			}
			if af == nil || len(af.PCM) == 0 || af.SampleRate != 48000 || af.Channels != 2 {
				t.Fatalf("invalid audio %+v", af)
			}
			deadline = time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				h.mu.Lock()
				keys, mouse := h.keys, h.mouse
				h.mu.Unlock()
				if keys > 0 && mouse > 0 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.keys == 0 || h.mouse == 0 {
				t.Fatal("input did not reach upstream")
			}
		})
	}
}
func TestSourceZGFXAndFragmentBounds(t *testing.T) {
	for _, size := range []int{1, 64, 65535, 65536, 140000} {
		p := bytes.Repeat([]byte{0x42}, size)
		var d zgfxDecoder
		out, e := d.decode(segmentGFX(p))
		if e != nil || !bytes.Equal(out, p) {
			t.Fatalf("ZGFX %d: %v", size, e)
		}
	}
	for _, p := range [][]byte{{}, {0xe0}, {0xe1, 0, 0}, {0xe0, 4, 1, 2}, {0xe0, 0x24, 0xff}} {
		var d zgfxDecoder
		_, e := d.decode(p)
		if len(p) == 4 && p[1] == 4 {
			if e != nil {
				t.Fatal(e)
			}
		} else if e == nil {
			t.Fatalf("malformed accepted %x", p)
		}
	}
}
func TestSourceInputScanRoundTrip(t *testing.T) {
	for _, code := range []uint8{4, 0x29, 0x58, 0xe0, 0xe4, 0xe7, 0x4f} {
		scan, ok := sourceScan(code)
		if !ok || hidKey(scan&0xff, scan&0x100 != 0) != code {
			t.Fatalf("usage %#x => %#x", code, scan)
		}
	}
}
func FuzzSourceGFX(f *testing.F) {
	f.Add(gfxPDU(9, []byte{1, 0, 1, 0, 1, 0, 0x20}))
	f.Fuzz(func(t *testing.T, p []byte) {
		s := sourceSession{conn: sourceDiscardConn{}, ready: make(chan struct{}), done: make(chan struct{}), frames: make(chan *capture.Frame, 2), surfaces: make(map[uint16]*sourceSurface)}
		if len(p) < 1<<20 {
			_ = s.handleSourceGFX(p)
		}
	})
}

type sourceDiscardConn struct{ discardConn }

func (sourceDiscardConn) SetReadDeadline(time.Time) error { return nil }

func TestSourceZGFXCompressedHistory(t *testing.T) {
	var d zgfxDecoder
	compressed := func(bits string) []byte { p := liteTestPacket(bits); p[1] = 0x24; return p }
	out, e := d.decode(compressed("0 01001001 10001 00001 110 001"))
	if e != nil || !bytes.Equal(out, bytes.Repeat([]byte{0x49}, 10)) {
		t.Fatal("ZGFX literal/match", e)
	}
	for _, n := range []int{8193, 32768, 65535} {
		out, e = d.decode(compressed("10001 00001 " + liteTestLength(n)))
		if e != nil || len(out) != n || out[n-1] != 0x49 {
			t.Fatalf("ZGFX long match %d: %v", n, e)
		}
	}
	history := bytes.Repeat([]byte{0x57}, 32768)
	if _, e = d.decode(segmentGFX(history)); e != nil {
		t.Fatal(e)
	}
	out, e = d.decode(compressed("101101 " + fmt.Sprintf("%015b", 32768-22176) + " 0"))
	if e != nil || !bytes.Equal(out, []byte{0x57, 0x57, 0x57}) {
		t.Fatal("ZGFX extended distance", e)
	}
}
