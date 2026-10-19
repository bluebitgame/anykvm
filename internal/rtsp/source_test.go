package rtsp

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"anykvm/internal/capture"
	"anykvm/internal/videodec"
)

func rtspFixture(t *testing.T) ([]byte, []byte, []byte) {
	t.Helper()
	p, e := os.ReadFile("../testdata/passthrough.h264")
	if e != nil {
		t.Fatal(e)
	}
	var sps, pps, idr []byte
	for _, n := range videodec.SplitNALs(p) {
		if len(n) == 0 {
			continue
		}
		switch n[0] & 31 {
		case 7:
			sps = n
		case 8:
			pps = n
		case 5:
			if idr == nil {
				idr = n
			}
		}
	}
	if len(sps)*len(pps)*len(idr) == 0 {
		t.Fatal("invalid fixture")
	}
	return sps, pps, idr
}
func testRTP(pt byte, seq uint16, stamp uint32, p []byte) []byte {
	h := make([]byte, 12)
	h[0] = 0x80
	h[1] = pt
	binary.BigEndian.PutUint16(h[2:], seq)
	binary.BigEndian.PutUint32(h[4:], stamp)
	return append(h, p...)
}
func interleaved(ch byte, p []byte) []byte {
	h := []byte{'$', ch, 0, 0}
	binary.BigEndian.PutUint16(h[2:], uint16(len(p)))
	return append(h, p...)
}
func TestRTSPSourceVideoAudioAndCredentials(t *testing.T) {
	sps, pps, idr := rtspFixture(t)
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	result := make(chan error, 1)
	go func() {
		conn, e := listener.Accept()
		if e != nil {
			result <- e
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		r := bufio.NewReader(conn)
		authenticated := false
		setups := 0
		for {
			line, e := r.ReadString('\n')
			if e != nil {
				result <- e
				return
			}
			fields := strings.Fields(line)
			if len(fields) != 3 {
				result <- fmt.Errorf("request %q", line)
				return
			}
			headers := map[string]string{}
			for {
				line, e = r.ReadString('\n')
				if e != nil {
					result <- e
					return
				}
				if line == "\r\n" {
					break
				}
				k, v, _ := strings.Cut(line, ":")
				headers[strings.ToLower(k)] = strings.TrimSpace(v)
			}
			if strings.Contains(fields[1], "secret") {
				result <- fmt.Errorf("credentials leaked into request URI")
				return
			}
			if !authenticated {
				if fields[0] != "DESCRIBE" {
					result <- fmt.Errorf("missing DESCRIBE")
					return
				}
				if headers["authorization"] == "" {
					fmt.Fprintf(conn, "RTSP/1.0 401 Unauthorized\r\nCSeq: %s\r\nWWW-Authenticate: Basic realm=\"test\"\r\n\r\n", headers["cseq"])
					continue
				}
				if headers["authorization"] != "Basic "+base64.StdEncoding.EncodeToString([]byte("user:secret")) {
					result <- fmt.Errorf("wrong credentials")
					return
				}
				authenticated = true
			}
			switch fields[0] {
			case "DESCRIBE":
				body := fmt.Sprintf("v=0\r\na=control:*\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\na=fmtp:96 packetization-mode=1;sprop-parameter-sets=%s,%s\r\na=control:video\r\nm=audio 0 RTP/AVP 0\r\na=control:audio\r\n", base64.StdEncoding.EncodeToString(sps), base64.StdEncoding.EncodeToString(pps))
				fmt.Fprintf(conn, "RTSP/1.0 200 OK\r\nCSeq: %s\r\nContent-Base: rtsp://%s/live/\r\nContent-Length: %d\r\n\r\n%s", headers["cseq"], listener.Addr(), len(body), body)
			case "SETUP":
				transport := headers["transport"]
				fmt.Fprintf(conn, "RTSP/1.0 200 OK\r\nCSeq: %s\r\nSession: test-session;timeout=60\r\nTransport: %s\r\n\r\n", headers["cseq"], transport)
				setups++
			case "PLAY":
				if setups != 2 {
					result <- fmt.Errorf("audio track not set up")
					return
				}
				fmt.Fprintf(conn, "RTSP/1.0 200 OK\r\nCSeq: %s\r\nSession: test-session\r\n\r\n", headers["cseq"])
				half := len(idr) / 2
				first := append([]byte{idr[0]&0xe0 | 28, 0x80 | idr[0]&31}, idr[1:half]...)
				last := append([]byte{idr[0]&0xe0 | 28, 0x40 | idr[0]&31}, idr[half:]...)
				for _, p := range [][]byte{interleaved(0, testRTP(96, 1, 9000, first)), interleaved(0, testRTP(96|128, 2, 9000, last)), interleaved(2, testRTP(0, 3, 800, []byte{0xff, 0x80, 0x00}))} {
					if _, e = conn.Write(p); e != nil {
						result <- e
						return
					}
				}
				_, _ = io.Copy(io.Discard, conn)
				result <- nil
				return
			default:
				result <- fmt.Errorf("unexpected method")
				return
			}
		}
	}()
	dev, ok := NewCapture("rtsp://user:secret@"+listener.Addr().String()+"/live", Options{Timeout: 3 * time.Second})
	if !ok {
		t.Fatal("source not selected")
	}
	d := dev.(*Device)
	d.SetVideoCodec("h264")
	if e = d.Open(); e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	f, e := d.Capture()
	if e != nil || f == nil || f.Format != capture.FormatH264 || f.Width != 64 || f.Height != 48 || !bytes.Contains(f.Data, idr) {
		t.Fatalf("video=%+v error=%v", f, e)
	}
	audio := d.AudioDevice()
	if e = audio.Open(); e != nil {
		t.Fatal(e)
	}
	af, e := audio.Capture()
	if e != nil || af == nil || len(af.PCM) != 3*6*4 || af.SampleRate != 48000 || af.Channels != 2 {
		t.Fatalf("audio=%+v error=%v", af, e)
	}
	audio.Close()
	if e = d.Close(); e != nil {
		t.Fatal(e)
	}
	if e = <-result; e != nil {
		t.Fatal(e)
	}
}
func TestRTPPacketLossWaitsForIDR(t *testing.T) {
	sps, pps, idr := rtspFixture(t)
	d := h264RTP{pt: 96}
	d.parameter(sps)
	d.parameter(pps)
	if p, _, _, e := d.push(testRTP(224, 1, 1, idr)); e != nil || len(p) == 0 {
		t.Fatal("IDR", e)
	}
	if p, _, _, e := d.push(testRTP(224, 3, 2, []byte{0x41, 0x80})); e != nil || len(p) != 0 {
		t.Fatal("prediction after packet loss", e)
	}
	if p, _, _, e := d.push(testRTP(224, 4, 3, idr)); e != nil || len(p) == 0 {
		t.Fatal("recovery", e)
	}
}
func TestAACMultipleAndFragmentedAUs(t *testing.T) {
	a := audioTrack{pt: 97, rate: 48000, channels: 2, codec: "MPEG4-GENERIC", config: []byte{0x11, 0x90}}
	p := []byte{0, 32, 0, 24, 0, 16, 1, 2, 3, 4, 5}
	frames, e := a.push(testRTP(97, 1, 100, p))
	if e != nil || len(frames) != 2 || !bytes.Equal(frames[1].Encoded, []byte{4, 5}) {
		t.Fatalf("AAC %v %v", frames, e)
	}
	frames, e = a.push(testRTP(97, 2, 200, []byte{0, 16, 0, 40, 1, 2}))
	if e != nil || len(frames) != 0 {
		t.Fatal("first AAC fragment", e)
	}
	frames, e = a.push(testRTP(97, 3, 200, []byte{0, 16, 0, 40, 3, 4, 5}))
	if e != nil || len(frames) != 1 || !bytes.Equal(frames[0].Encoded, []byte{1, 2, 3, 4, 5}) {
		t.Fatal("AAC fragment completion", e)
	}
}
func TestDigestAuthenticationVector(t *testing.T) {
	s := session{auth: `Digest realm="test", nonce="abc", qop="auth,auth-int"`, password: "secret"}
	auth, e := s.authorization("DESCRIBE", "rtsp://host/live", "user")
	if e != nil || !strings.Contains(auth, "qop=auth") || !strings.Contains(auth, "nc=00000001") {
		t.Fatal(auth, e)
	}
	if _, e = controlURL("rtsp://host/live/", "rtsp://other/live"); e == nil {
		t.Fatal("cross-origin control accepted")
	}
}
func FuzzRTP(f *testing.F) {
	f.Add(testRTP(224, 1, 1, []byte{0x65, 1}))
	f.Fuzz(func(t *testing.T, p []byte) {
		var d h264RTP
		_, _, _, _ = d.push(p)
		a := audioTrack{pt: 97, codec: "MPEG4-GENERIC", rate: 48000}
		_, _ = a.push(p)
	})
}
