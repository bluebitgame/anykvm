package ard

import (
	"bytes"
	"crypto/aes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"anykvm/internal/capture"
)

func TestAddressAndCredentialPolicy(t *testing.T) {
	for id, want := range map[string]string{"ard://mac": "mac:5900", "ard://[::1]:5901": "[::1]:5901", "ARD://mac:5900": "mac:5900"} {
		got, err := ParseAddress(id)
		if err != nil || got != want {
			t.Fatalf("%s: %s %v", id, got, err)
		}
	}
	for _, id := range []string{"ard://user:secret@mac", "ard://mac?password=secret", "ard://mac/path", "ard://mac:99999", "ard://mac#secret", "ard://mac:bad", "ard://"} {
		_, err := ParseAddress(id)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe/accepted URI %s: %v", id, err)
		}
	}
	for _, o := range []Options{{Codec: "av1"}, {DisplayMode: "virtual"}, {Fallback: "auto"}, {Timeout: -1}, {Password: "p", PasswordEnv: "PW"}, {Username: strings.Repeat("x", 64)}} {
		if o.Validate() == nil {
			t.Fatalf("accepted %+v", o)
		}
	}
	t.Setenv("ANYKVM_ARD_TEST_PASSWORD", "pass")
	o := Options{Username: "user", PasswordEnv: "ANYKVM_ARD_TEST_PASSWORD"}
	p, err := o.credentials()
	if err != nil || p != "pass" {
		t.Fatal(err)
	}
	if (Options{}).normalized().Fallback != "deny" {
		t.Fatal("fallback must be explicit")
	}
	c, _ := NewClient("ard://127.0.0.1", Options{Username: "u", Password: "p", Codec: "h264"})
	if err := c.Open(); !errors.Is(err, ErrUnsupported) {
		t.Fatal("unverified H.264 advertised", err)
	}
}

func TestControlEncryptionAndIntegrity(t *testing.T) {
	key, iv := bytes.Repeat([]byte{42}, 16), bytes.Repeat([]byte{9}, 16)
	send, _ := newControlCipher(key, iv)
	recv, _ := newControlCipher(key, iv)
	for i := 0; i < 5; i++ {
		p := bytes.Repeat([]byte{byte(i)}, 7+i*31)
		encrypted, err := send.encrypt(p)
		if err != nil {
			t.Fatal(err)
		}
		plain, err := recv.decrypt(encrypted)
		if err != nil || !bytes.Equal(plain, p) {
			t.Fatal("continuous CBC roundtrip", err)
		}
	}
	encrypted, _ := send.encrypt([]byte("message"))
	encrypted[16] ^= 1
	if _, err := recv.decrypt(encrypted); err == nil {
		t.Fatal("accepted corrupt record")
	}
	for n := 0; n < 40; n++ {
		p := bytes.Repeat([]byte{1}, n)
		encrypted, err := legacyEncrypt(key, p)
		if err != nil {
			t.Fatal(err)
		}
		plain, err := legacyDecrypt(key, encrypted)
		if err != nil || !bytes.Equal(plain, p) {
			t.Fatal("legacy CBC roundtrip", err)
		}
	}
}

// Independent cipher-suite-5 compatibility vector from rootshell-vnc at commit
// 3950a452ad6ed75f82177964418092d1fa88a929, Tests/RFBTransportTests/AppleSRTPContextTests.swift.
// MIT terms: third_party/rootshell-vnc/LICENSE, restored by tools/prepare_sources.py.
func TestSRTPCompatibilityVectorAndReplay(t *testing.T) {
	decode := func(s string) []byte {
		p, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	key := decode("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8gISIjJCUmJygpKissLQ==")
	packet := decode("kGASNAECAwQRIjNEvt4AAaChoqP5uBi2sP75CY69L33Z/+U/WzaVmrZS0yJWMzsBZefJem06t3Hqh9pN49XoRfBwPuON97n+31NigxWW")
	expected := decode("kGASNAECAwQRIjNEvt4AAaChoqMAAQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHyAhIiMkJSYnKCkqKywtLi8=")
	crypto, err := newMediaCrypto(key, 0)
	if err != nil {
		t.Fatal(err)
	}
	receiver := &srtpReceiver{mediaCrypto: crypto}
	plain, err := receiver.unprotect(packet)
	if err != nil || !bytes.Equal(plain, expected) {
		t.Fatalf("SRTP interoperability: %v", err)
	}
	if _, err := receiver.unprotect(packet); err == nil {
		t.Fatal("accepted replay")
	}
	packet[len(packet)-1] ^= 1
	fresh := &srtpReceiver{mediaCrypto: crypto}
	if _, err := fresh.unprotect(packet); err == nil {
		t.Fatal("accepted corrupt tag")
	}
}

func serveMacAuth(conn net.Conn, key *rsa.PrivateKey, version string, reject bool) error {
	return serveMacAuthSized(conn, key, version, reject, 2, 1, nil)
}
func serveMacAuthSized(conn net.Conn, key *rsa.PrivateKey, version string, reject bool, width, height int, out *[]byte) error {
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, version); err != nil {
		return err
	}
	if _, err := readExactly(conn, 12); err != nil {
		return err
	}
	if _, err := conn.Write([]byte{1, 33}); err != nil {
		return err
	}
	p, err := readExactly(conn, 15)
	if err != nil {
		return err
	}
	if !bytes.Equal(p, []byte{33, 0, 0, 0, 10, 1, 0, 'R', 'S', 'A', '1', 0, 0, 0, 0}) {
		return fmt.Errorf("bad RSA1 request")
	}
	der := x509.MarshalPKCS1PublicKey(&key.PublicKey)
	body := []byte{1, 0}
	body = binary.BigEndian.AppendUint32(body, uint32(len(der)))
	body = append(body, der...)
	body = append(body, 0)
	if _, err = conn.Write(append(binary.BigEndian.AppendUint32(nil, uint32(len(body))), body...)); err != nil {
		return err
	}
	h, err := readExactly(conn, 4)
	if err != nil {
		return err
	}
	p, err = readExactly(conn, int(be32(h)))
	if err != nil {
		return err
	}
	if len(p) < 138 || !bytes.Equal(p[:8], []byte{1, 0, 'R', 'S', 'A', '1', 0, 1}) || int(binary.LittleEndian.Uint16(p[136:])) != len(p)-138 {
		return fmt.Errorf("bad RSA authentication layout")
	}
	sessionKey, err := rsa.DecryptPKCS1v15(rand.Reader, key, p[138:])
	if err != nil {
		return err
	}
	if out != nil {
		*out = bytes.Clone(sessionKey)
	}
	block, err := aes.NewCipher(sessionKey)
	if err != nil {
		return err
	}
	creds := bytes.Clone(p[8:136])
	for off := 0; off < 128; off += 16 {
		block.Decrypt(creds[off:off+16], creds[off:off+16])
	}
	if string(bytes.SplitN(creds[:64], []byte{0}, 2)[0]) != "tester" || string(bytes.SplitN(creds[64:], []byte{0}, 2)[0]) != "test-password" {
		return fmt.Errorf("wrong decrypted credentials")
	}
	if reject {
		_, err = conn.Write([]byte{0, 0, 0, 1})
		return err
	}
	if _, err = conn.Write(make([]byte, 8)); err != nil {
		return err
	}
	if _, err = readExactly(conn, 1); err != nil {
		return err
	}
	init := make([]byte, 24)
	put16(init, width)
	put16(init[2:], height)
	put32(init[20:], 4)
	_, err = conn.Write(append(init, []byte("test")...))
	return err
}

func TestAppleDHAuthentication(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	s := &session{conn: client, reader: client}
	serverErr := make(chan error, 1)
	go func() {
		prime := new(big.Int)
		prime.SetString("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFF61", 16)
		pub := new(big.Int).Exp(big.NewInt(2), big.NewInt(7), prime)
		p := []byte{0, 2, 0, 16}
		p = append(p, prime.FillBytes(make([]byte, 16))...)
		p = append(p, pub.FillBytes(make([]byte, 16))...)
		if _, err := server.Write(p); err != nil {
			serverErr <- err
			return
		}
		r, err := readExactly(server, 144)
		if err != nil {
			serverErr <- err
			return
		}
		clientPub := new(big.Int).SetBytes(r[128:])
		if clientPub.Sign() == 0 {
			serverErr <- fmt.Errorf("empty client public key")
			return
		}
		// Verify the transmitted credentials with the independently computed server secret.
		secret := new(big.Int).Exp(clientPub, big.NewInt(7), prime)
		serverErr <- verifyDHCredentials(secret.Bytes(), r[:128])
	}()
	if err := s.authDH("tester", "test-password"); err != nil {
		t.Fatal(err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestAccountFallbackLifecycleAndInput(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var connections atomic.Int32
	message := make(chan []byte, 64)
	serverErr := make(chan error, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer conn.Close()
				if err := serveMacAuth(conn, key, "RFB 003.008\n", false); err != nil {
					if !errors.Is(err, io.EOF) {
						serverErr <- err
					}
					return
				}
				serverErr <- serveStandard(conn, message)
			}()
		}
	}()
	c, err := NewClient("ard://"+ln.Addr().String(), Options{Username: "tester", Password: "test-password", Fallback: "vnc", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 2; round++ {
		if err := c.Open(); err != nil {
			t.Fatal(err)
		}
		f, err := c.Capture()
		if err != nil || f == nil || f.Width != 2 || !bytes.Equal(f.Data, []byte{1, 2, 3, 255, 4, 5, 6, 255}) {
			t.Fatalf("frame=%+v err=%v", f, err)
		}
		if idle, _ := c.CaptureIdleStatus(); !idle {
			t.Fatal("static desktop is not healthy")
		}
		if err := c.KeyDown(0x04); err != nil {
			t.Fatal(err)
		}
		c.MouseMove(32767, 32767)
		c.MouseButton(2, true)
		c.MouseWheel(1)
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Capture(); !errors.Is(err, capture.ErrStreamStopped) {
			t.Fatal("closed capture", err)
		}
	}
	if connections.Load() != 4 {
		t.Fatalf("fallback must reconnect explicitly: %d", connections.Load())
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-serverErr:
			if err != nil && !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("peer workers did not close")
		}
	}
	var releasedKey, releasedButton, rightButton bool
	for len(message) > 0 {
		p := <-message
		if p[0] == 4 && p[1] == 0 && be32(p[4:]) == 'a' {
			releasedKey = true
		}
		if p[0] == 5 && p[1] == 4 {
			rightButton = true
		}
		if p[0] == 5 && p[1] == 0 {
			releasedButton = true
		}
	}
	if !releasedKey || !releasedButton || !rightButton {
		t.Fatalf("input lifecycle: key=%v button=%v right=%v", releasedKey, releasedButton, rightButton)
	}
	for len(serverErr) > 0 {
		if err := <-serverErr; err != nil && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
	}
}

func TestAuthenticationRejectionDoesNotFallback(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var connections atomic.Int32
	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		connections.Add(1)
		defer conn.Close()
		done <- serveMacAuth(conn, key, "RFB 003.889\n", true)
	}()
	c, _ := NewClient("ard://"+ln.Addr().String(), Options{Username: "tester", Password: "test-password", Fallback: "vnc", Timeout: time.Second})
	if err := c.Open(); err == nil || errors.Is(err, ErrUnsupported) {
		t.Fatal("authentication failure was treated as a capability downgrade", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if connections.Load() != 1 {
		t.Fatal("authentication rejection caused a fallback connection")
	}
}

func serveStandard(conn net.Conn, messages chan []byte) error {
	for {
		p, err := readExactly(conn, 1)
		if err != nil {
			return err
		}
		var n int
		switch p[0] {
		case 0:
			n = 19
		case 2:
			h, err := readExactly(conn, 3)
			if err != nil {
				return err
			}
			n = int(be16(h[1:])) * 4
		case 3:
			n = 9
		case 4:
			n = 7
		case 5:
			n = 5
		default:
			return fmt.Errorf("unexpected client message %d", p[0])
		}
		rest, err := readExactly(conn, n)
		if err != nil {
			return err
		}
		p = append(p, rest...)
		if p[0] == 3 && p[1] == 0 {
			update := []byte{0, 0, 0, 1}
			r := make([]byte, 12)
			put16(r[4:], 2)
			put16(r[6:], 1)
			update = append(update, r...)
			update = append(update, 1, 2, 3, 255, 4, 5, 6, 255)
			if _, err = conn.Write(update); err != nil {
				return err
			}
		}
		if p[0] == 4 || p[0] == 5 {
			messages <- p
		}
	}
}
