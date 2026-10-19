package gamestream

import (
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type wireReply struct {
	Code        int    `xml:"status_code,attr"`
	Paired      string `xml:"paired"`
	PairStatus  string `xml:"PairStatus"`
	Certificate string `xml:"plaincert"`
	Challenge   string `xml:"challengeresponse"`
	Secret      string `xml:"pairingsecret"`
	ID          string `xml:"uniqueid"`
	Game        string `xml:"currentgame"`
	State       string `xml:"state"`
	Apps        []struct {
		Title string `xml:"AppTitle"`
		ID    int    `xml:"ID"`
	} `xml:"App"`
}

func getReply(t *testing.T, client *http.Client, base, path string, q url.Values) wireReply {
	t.Helper()
	resp, err := client.Get(base + path + "?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var reply wireReply
	if err = xml.NewDecoder(resp.Body).Decode(&reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

func testServer(t *testing.T) *Server {
	t.Helper()
	s, err := NewServer(Config{HTTPAddr: "127.0.0.1:0", HTTPSAddr: "127.0.0.1:0", StateFile: filepath.Join(t.TempDir(), "state.json"), Machines: []string{"Desktop & <KVM>", "Laptop"}, RequestPIN: func(context.Context, PairingRequest) (string, error) { return "1234", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func plainURL(s *Server) string  { return fmt.Sprintf("http://127.0.0.1:%d", s.httpPort) }
func secureURL(s *Server) string { return fmt.Sprintf("https://127.0.0.1:%d", s.httpsPort) }

// Test client independently implements the five GameStream pairing phases.
// It validates the host's proof as well as completing its own.
type pairingClient struct {
	cert                        tls.Certificate
	pem                         []byte
	key                         *rsa.PrivateKey
	leaf                        *x509.Certificate
	aesKey                      []byte
	challenge, secret           []byte
	serverCert                  *x509.Certificate
	serverHash, serverChallenge []byte
	q                           url.Values
}

func newClient(t *testing.T) *pairingClient {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(42), Subject: pkix.Name{CommonName: "GameStream test client"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &pairingClient{key: key, leaf: leaf, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, challenge: bytes.Repeat([]byte{0x31}, 16), secret: bytes.Repeat([]byte{0x72}, 16), q: url.Values{"uniqueid": {"test-client"}, "devicename": {"GameStream test"}}}
}

func clientAES(t *testing.T, key, in []byte, encrypt bool) []byte {
	t.Helper()
	b, err := aes.NewCipher(key)
	if err != nil || len(in)%16 != 0 {
		t.Fatalf("invalid client AES input: %v", err)
	}
	out := make([]byte, len(in))
	for i := 0; i < len(in); i += 16 {
		if encrypt {
			b.Encrypt(out[i:i+16], in[i:i+16])
		} else {
			b.Decrypt(out[i:i+16], in[i:i+16])
		}
	}
	return out
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	out, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (c *pairingClient) call(t *testing.T, s *Server, name, value string) wireReply {
	t.Helper()
	q := url.Values{"uniqueid": {c.q.Get("uniqueid")}, name: {value}}
	return getReply(t, http.DefaultClient, plainURL(s), "/pair", q)
}

func (c *pairingClient) begin(t *testing.T, s *Server, pin string) {
	t.Helper()
	salt := bytes.Repeat([]byte{0x19}, 16)
	key := sha256.Sum256(append(append([]byte{}, salt...), []byte(pin)...))
	c.aesKey = key[:16]
	q := url.Values{"uniqueid": {c.q.Get("uniqueid")}, "devicename": {"GameStream test"}, "phrase": {"getservercert"}, "salt": {hex.EncodeToString(salt)}, "clientcert": {hex.EncodeToString(c.pem)}}
	r := getReply(t, http.DefaultClient, plainURL(s), "/pair", q)
	if r.Code != 200 || r.Paired != "1" {
		t.Fatalf("getservercert: %+v", r)
	}
	block, _ := pem.Decode(unhex(t, r.Certificate))
	if block == nil {
		t.Fatal("missing server certificate")
	}
	var err error
	c.serverCert, err = x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
}

func (c *pairingClient) challengeHost(t *testing.T, s *Server) {
	t.Helper()
	r := c.call(t, s, "clientchallenge", hex.EncodeToString(clientAES(t, c.aesKey, c.challenge, true)))
	if r.Code != 200 {
		t.Fatalf("clientchallenge: %+v", r)
	}
	data := clientAES(t, c.aesKey, unhex(t, r.Challenge), false)
	if len(data) != 48 {
		t.Fatalf("challenge length: %d", len(data))
	}
	c.serverHash, c.serverChallenge = data[:32], data[32:]
}

func (c *pairingClient) answerHost(t *testing.T, s *Server, verifyHost bool) {
	t.Helper()
	material := append(append(append([]byte{}, c.serverChallenge...), c.leaf.Signature...), c.secret...)
	h := sha256.Sum256(material)
	r := c.call(t, s, "serverchallengeresp", hex.EncodeToString(clientAES(t, c.aesKey, h[:], true)))
	if r.Code != 200 {
		t.Fatalf("serverchallengeresp: %+v", r)
	}
	data := unhex(t, r.Secret)
	if len(data) != 272 {
		t.Fatalf("server proof length: %d", len(data))
	}
	digest := sha256.Sum256(data[:16])
	if err := rsa.VerifyPKCS1v15(c.serverCert.PublicKey.(*rsa.PublicKey), crypto.SHA256, digest[:], data[16:]); err != nil {
		t.Fatal(err)
	}
	material = append(append(append([]byte{}, c.challenge...), c.serverCert.Signature...), data[:16]...)
	expected := sha256.Sum256(material)
	if verifyHost && !bytes.Equal(expected[:], c.serverHash) {
		t.Fatal("server challenge proof does not match PIN")
	}
}

func (c *pairingClient) finish(t *testing.T, s *Server, tamper bool) wireReply {
	t.Helper()
	h := sha256.Sum256(c.secret)
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, h[:])
	if err != nil {
		t.Fatal(err)
	}
	if tamper {
		sig[0] ^= 1
	}
	return c.call(t, s, "clientpairingsecret", hex.EncodeToString(append(append([]byte{}, c.secret...), sig...)))
}

func tlsClient(t *testing.T, s *Server, cert *tls.Certificate) *http.Client {
	t.Helper()
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true, // GameStream uses certificate pinning, not DNS/CA validation.
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) != 1 || !bytes.Equal(cs.PeerCertificates[0].Raw, s.cert.Certificate[0]) {
				return errors.New("server certificate pin mismatch")
			}
			return nil
		},
	}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	tr := &http.Transport{TLSClientConfig: cfg}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 5 * time.Second}
}

func pairClient(t *testing.T, s *Server) *pairingClient {
	t.Helper()
	c := newClient(t)
	c.begin(t, s, "1234")
	c.challengeHost(t, s)
	c.answerHost(t, s, true)
	if r := c.finish(t, s, false); r.Code != 200 || r.Paired != "1" {
		t.Fatalf("clientpairingsecret: %+v", r)
	}
	return c
}

func TestPairingPersistenceAndAuthorization(t *testing.T) {
	s := testServer(t)
	info := getReply(t, http.DefaultClient, plainURL(s), "/serverinfo", nil)
	if info.Code != 200 || info.ID == "" || info.PairStatus != "0" {
		t.Fatalf("serverinfo: %+v", info)
	}
	c := pairClient(t, s)
	https := tlsClient(t, s, &c.cert)
	r := getReply(t, https, secureURL(s), "/pair", url.Values{"phrase": {"pairchallenge"}})
	if r.Code != 200 || r.Paired != "1" {
		t.Fatalf("final TLS challenge: %+v", r)
	}
	r = getReply(t, https, secureURL(s), "/applist", nil)
	if r.Code != 200 || len(r.Apps) != 2 || r.Apps[0].Title != "Desktop & <KVM>" {
		t.Fatalf("applist: %+v", r)
	}
	appID := r.Apps[0].ID
	r = getReply(t, https, secureURL(s), "/launch", url.Values{"appid": {fmt.Sprint(appID)}})
	if r.Code != 503 {
		t.Fatalf("unimplemented launch must fail explicitly: %+v", r)
	}
	r = getReply(t, https, secureURL(s), "/serverinfo", nil)
	if r.Game != "0" || r.PairStatus != "1" || r.State != "SUNSHINE_SERVER_FREE" {
		t.Fatalf("failed launch changed state: %+v", r)
	}
	// uniqueid is caller-controlled and cannot impersonate a paired certificate.
	for _, path := range []string{"/applist", "/launch", "/resume", "/cancel"} {
		if r := getReply(t, http.DefaultClient, plainURL(s), path, c.q); r.Code != 401 {
			t.Fatalf("plaintext %s: %+v", path, r)
		}
	}
	if r := getReply(t, http.DefaultClient, plainURL(s), "/pair", url.Values{"phrase": {"pairchallenge"}}); r.Code != 401 {
		t.Fatal("plaintext pairchallenge accepted")
	}
	for _, cert := range []*tls.Certificate{nil, &newClient(t).cert} {
		if r := getReply(t, tlsClient(t, s, cert), secureURL(s), "/applist", c.q); r.Code != 401 {
			t.Fatal("unauthorized TLS certificate accepted")
		}
	}
	// Plaintext cleanup must not revoke an established pairing.
	getReply(t, http.DefaultClient, plainURL(s), "/unpair", c.q)
	if r := getReply(t, https, secureURL(s), "/applist", nil); r.Code != 200 {
		t.Fatal("HTTP unpair revoked an authorized client")
	}
	s.Close()
	cfg := s.cfg
	cfg.RequestPIN = nil
	cfg.Machines = []string{"Laptop", "Desktop & <KVM>"}
	s2, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = s2.Start(); err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if !bytes.Equal(s.cert.Certificate[0], s2.cert.Certificate[0]) {
		t.Fatal("host certificate changed on restart")
	}
	https = tlsClient(t, s2, &c.cert)
	r = getReply(t, https, secureURL(s2), "/applist", nil)
	if r.Code != 200 || r.Apps[1].ID != appID {
		t.Fatalf("restart/reorder lost identity: %+v", r)
	}
	if r := getReply(t, https, secureURL(s2), "/unpair", nil); r.Code != 200 {
		t.Fatal("authenticated unpair failed")
	}
	if r := getReply(t, https, secureURL(s2), "/applist", nil); r.Code != 401 {
		t.Fatal("revoked certificate retained access on existing connection")
	}
	state, _, err := loadIdentity(cfg.StateFile)
	if err != nil || len(state.Clients) != 0 || state.ID != info.ID {
		t.Fatalf("revocation not persisted: %v", err)
	}
	stat, err := os.Stat(cfg.StateFile)
	if err != nil || stat.Mode().Perm()&0077 != 0 {
		t.Fatalf("identity permissions: %v, %v", stat, err)
	}
}

func TestPairingRejectsBadProofAndPersistsOnlyAfterSuccess(t *testing.T) {
	for _, mode := range []string{"wrong PIN", "tampered signature", "disk failure"} {
		t.Run(mode, func(t *testing.T) {
			s := testServer(t)
			c := newClient(t)
			pin := "1234"
			if mode == "wrong PIN" {
				pin = "5678"
			}
			c.begin(t, s, pin)
			c.challengeHost(t, s)
			c.answerHost(t, s, mode != "wrong PIN")
			if mode == "disk failure" {
				s.cfg.StateFile = filepath.Join(s.cfg.StateFile, "unwritable.json")
			}
			r := c.finish(t, s, mode == "tampered signature")
			if r.Code == 200 || r.Paired != "0" {
				t.Fatalf("bad proof/storage accepted: %+v", r)
			}
			if s.authorized(&http.Request{TLS: &tls.ConnectionState{PeerCertificates: []*x509.Certificate{c.leaf}}}) {
				t.Fatal("failed pairing authorized client")
			}
			if s.pairing != nil {
				t.Fatal("failed pairing session retained")
			}
		})
	}
}

func TestPairingOrderExpiryAndBounds(t *testing.T) {
	s := testServer(t)
	c := newClient(t)
	c.begin(t, s, "1234")
	if r := c.call(t, s, "serverchallengeresp", strings.Repeat("00", 32)); r.Code != 400 {
		t.Fatal("out-of-order proof accepted")
	}
	c.begin(t, s, "1234")
	if r := c.call(t, s, "clientchallenge", "aa"); r.Code != 400 {
		t.Fatal("short AES block accepted")
	}
	c.begin(t, s, "1234")
	s.mu.Lock()
	s.pairing.expires = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if r := c.call(t, s, "clientchallenge", strings.Repeat("00", 16)); r.Code != 400 {
		t.Fatal("expired pairing accepted")
	}
	if r := getReply(t, http.DefaultClient, plainURL(s), "/pair", url.Values{"uniqueid": {"one", "two"}}); r.Code != 400 {
		t.Fatal("duplicate identity accepted")
	}
	if r := getReply(t, http.DefaultClient, plainURL(s), "/pair", url.Values{"uniqueid": {strings.Repeat("x", 13000)}}); r.Code != 400 {
		t.Fatal("oversized query accepted")
	}
}

func TestPendingPairCancellation(t *testing.T) {
	s := testServer(t)
	entered := make(chan struct{})
	returned := make(chan struct{})
	s.cfg.RequestPIN = func(ctx context.Context, _ PairingRequest) (string, error) {
		close(entered)
		<-ctx.Done()
		close(returned)
		return "", ctx.Err()
	}
	c := newClient(t)
	q := url.Values{"uniqueid": {"waiting"}, "phrase": {"getservercert"}, "salt": {strings.Repeat("00", 16)}, "clientcert": {hex.EncodeToString(c.pem)}}
	done := make(chan error, 1)
	go func() {
		resp, err := http.Get(plainURL(s) + "/pair?" + q.Encode())
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("no pairing prompt")
	}
	if r := getReply(t, http.DefaultClient, plainURL(s), "/pair", q); r.Code != 409 {
		t.Fatal("parallel pairing accepted")
	}
	getReply(t, http.DefaultClient, plainURL(s), "/unpair", url.Values{"uniqueid": {"waiting"}})
	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not release PIN callback")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCorruptStateAndListenerRollback(t *testing.T) {
	s := testServer(t)
	s.Close()
	if err := s.Start(); err == nil {
		t.Fatal("closed server restarted")
	}
	if err := os.WriteFile(s.cfg.StateFile, []byte("invalid state"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewServer(s.cfg); err == nil {
		t.Fatal("corrupt identity silently regenerated")
	}
	data, _ := os.ReadFile(s.cfg.StateFile)
	if string(data) != "invalid state" {
		t.Fatal("corrupt identity overwritten")
	}
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := free.Addr().String()
	free.Close()
	s2, err := NewServer(Config{HTTPAddr: addr, HTTPSAddr: busy.Addr().String(), StateFile: filepath.Join(t.TempDir(), "state.json"), Machines: []string{"test"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := s2.Start(); err == nil {
		t.Fatal("occupied HTTPS port accepted")
	}
	check, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("HTTP listener leaked: %v", err)
	}
	check.Close()
}

func TestPairingAESKnownVector(t *testing.T) {
	// NIST SP 800-38A F.1.1 AES-128 ECB first block.
	key := unhex(t, "2b7e151628aed2a6abf7158809cf4f3c")
	plain := unhex(t, "6bc1bee22e409f96e93d7e117393172a")
	want := unhex(t, "3ad77bb40d7a3660a89ecaf32466ef97")
	got, err := cryptPairing(key, plain, true)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("AES vector: %x %v", got, err)
	}
	got, err = cryptPairing(key, want, false)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatal("AES decrypt vector")
	}
}
