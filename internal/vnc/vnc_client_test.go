package vnc

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"anykvm/internal/capture"
)

func TestVNCClientRoundTrip(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		name := "plain"
		if encrypted {
			name = "VeNCrypt"
		}
		t.Run(name, func(t *testing.T) {
			pixels := []byte{1, 2, 3, 255, 4, 5, 6, 255, 7, 8, 9, 255, 10, 11, 12, 255}
			srv := NewServer(Config{Addr: "127.0.0.1:0", Width: 2, Height: 2, Name: string(bytes.Repeat([]byte("n"), 300)), Password: "test"})
			if encrypted {
				key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
				der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
				if err != nil {
					t.Fatal(err)
				}
				srv.tlsConfig = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
			}
			srv.config.OnClientConnected = func() { srv.PushFrame(Frame{Format: capture.FormatRaw, Data: pixels, Width: 2, Height: 2}) }
			if err := srv.Start(); err != nil {
				t.Fatal(err)
			}
			defer srv.Close()
			client, err := NewVNCClient("vnc://test@" + srv.ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			if err := client.Open(); err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			select {
			case f := <-client.frameCh:
				if f.Width != 2 || f.Height != 2 || !bytes.Equal(f.Data, pixels) {
					t.Fatalf("unexpected frame: %+v", f)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("no frame received")
			}
			if len(client.fbName) != 300 {
				t.Fatalf("desktop name length = %d", len(client.fbName))
			}
		})
	}
}

func rectangleHeader(x, y, w, h uint16, encoding int32) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint16(b[0:], x)
	binary.BigEndian.PutUint16(b[2:], y)
	binary.BigEndian.PutUint16(b[4:], w)
	binary.BigEndian.PutUint16(b[6:], h)
	binary.BigEndian.PutUint32(b[8:], uint32(encoding))
	return b
}

func TestVNCDesktopResizeHasNoPayload(t *testing.T) {
	b := []byte{0, 0, 2}
	b = append(b, rectangleHeader(0, 0, 2, 1, encDesktopSize)...)
	b = append(b, rectangleHeader(0, 0, 2, 1, encRaw)...)
	pixels := []byte{1, 2, 3, 255, 4, 5, 6, 255}
	b = append(b, pixels...)
	c := &VNCClient{reader: bytes.NewReader(b), pf: defaultPixelFormat(), fbWidth: 1, fbHeight: 1, fb: make([]byte, 4), frameCh: make(chan *capture.Frame, 1)}
	if err := c.handleFramebufferUpdate(); err != nil {
		t.Fatal(err)
	}
	f := <-c.frameCh
	if f.Width != 2 || f.Height != 1 || !bytes.Equal(f.Data, pixels) {
		t.Fatalf("unexpected resized frame: %+v", f)
	}
}

func TestVNCRejectsOutOfBoundsRectangle(t *testing.T) {
	b := append([]byte{0, 0, 1}, rectangleHeader(1, 0, 2, 1, encRaw)...)
	c := &VNCClient{reader: bytes.NewReader(b), pf: defaultPixelFormat(), fbWidth: 2, fbHeight: 1, fb: make([]byte, 8)}
	if err := c.handleFramebufferUpdate(); err == nil {
		t.Fatal("expected rectangle bounds error")
	}
}

func TestCopyRectOverlappingRows(t *testing.T) {
	fb := []byte{1, 1, 1, 1, 2, 2, 2, 2, 3, 3, 3, 3}
	decodeCopyRectClient(fb, 1, rect{y: 1, width: 1, height: 2}, []byte{0, 0, 0, 0})
	want := []byte{1, 1, 1, 1, 1, 1, 1, 1, 2, 2, 2, 2}
	if !bytes.Equal(fb, want) {
		t.Fatalf("got %v, want %v", fb, want)
	}
}

func TestVNCInputButtonMappingAndKeyClear(t *testing.T) {
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	c := &VNCClient{conn: conn, running: true, closed: make(chan struct{})}
	messages := make(chan []byte, 3)
	go func() {
		for _, size := range []int{6, 8, 8} {
			b := make([]byte, size)
			for n := 0; n < size; {
				read, err := peer.Read(b[n:])
				if err != nil {
					return
				}
				n += read
			}
			messages <- b
		}
	}()
	c.MouseButton(2, true)
	c.KeyDown(0x04)
	c.KeyUp()
	if b := <-messages; b[1] != 4 {
		t.Fatalf("right button mask = %d", b[1])
	}
	<-messages
	if b := <-messages; b[1] != 0 {
		t.Fatal("KeyUp did not release the held key")
	}
}

func TestH264BackpressureCanBeCancelled(t *testing.T) {
	c := &client{wake: make(chan struct{}, 1), done: make(chan struct{})}
	c.h264QCond = sync.NewCond(&c.stateMu)
	fd := &frameData{format: capture.FormatH264}
	c.offerFrame(fd)
	c.offerFrame(fd)
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() { c.offerFrameWithStop(fd, stop); close(finished) }()
	close(stop)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("capture cancellation did not unblock the full H.264 queue")
	}
}

func TestVNCDoesNotDowngradeInvalidTLS(t *testing.T) {
	for _, cfg := range []Config{
		{Addr: "127.0.0.1:0", VNCTLS: true},
		{Addr: "127.0.0.1:0", VNCTLS: true, TLSCertFile: "missing.pem", TLSKeyFile: "missing.key"},
	} {
		srv := NewServer(cfg)
		if err := srv.Start(); err == nil {
			srv.Close()
			t.Fatal("TLS configuration failure silently enabled plaintext")
		}
	}
}
