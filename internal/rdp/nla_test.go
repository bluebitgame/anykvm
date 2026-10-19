package rdp

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rc4"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"io"
	"math/big"
	"net"
	"testing"
	"time"
)

func testNLACertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func testNLANegotiate(flags uint32) []byte {
	n := 32
	if flags&ntlmVersion != 0 {
		n = 40
	}
	p := make([]byte, n)
	copy(p, ntlmSignature)
	le.PutUint32(p[8:], 1)
	le.PutUint32(p[12:], flags)
	if n == 40 {
		copy(p[32:], []byte{6, 1, 0, 0, 0, 0, 0, 15})
	}
	return p
}

func testNLAAuthenticate(t *testing.T, negotiate, challenge []byte, user, pass string) ([]byte, *ntlmSecurity) {
	t.Helper()
	flags := le.Uint32(challenge[20:])
	info, err := ntlmField(challenge, 40, 48)
	if err != nil {
		t.Fatal(err)
	}
	var timestamp []byte
	for avs := info; len(avs) >= 4; {
		id, n := le.Uint16(avs), int(le.Uint16(avs[2:]))
		if id == 7 {
			timestamp = avs[4 : 4+n]
		}
		avs = avs[4+n:]
	}
	domain := "ANYKVM"
	blob := join([]byte{1, 1, 0, 0, 0, 0, 0, 0}, timestamp, bytes.Repeat([]byte{0x42}, 8), make([]byte, 4), info, make([]byte, 4))
	key := ntlmResponseKey(user, domain, pass)
	proof := hmacMD5(key, challenge[24:32], blob)
	key = hmacMD5(key, proof)
	var encryptedKey []byte
	if flags&ntlmKeyExch != 0 {
		exported := bytes.Repeat([]byte{0x37}, 16)
		cipher, _ := rc4.NewCipher(key)
		encryptedKey = make([]byte, 16)
		cipher.XORKeyStream(encryptedKey, exported)
		key = exported
	}
	micOff := 64
	if flags&ntlmVersion != 0 {
		micOff += 8
	}
	p := make([]byte, micOff+16)
	copy(p, ntlmSignature)
	le.PutUint32(p[8:], 3)
	le.PutUint32(p[60:], flags)
	if flags&ntlmVersion != 0 {
		copy(p[64:], negotiate[32:40])
	}
	for i, field := range [][]byte{make([]byte, 24), join(proof, blob), utf16Bytes(domain), utf16Bytes(user), nil, encryptedKey} {
		putNTLMField(p, 12+i*8, len(p), len(field))
		p = append(p, field...)
	}
	copy(p[micOff:], hmacMD5(key, negotiate, challenge, p))
	security := newNTLMSecurity(key, flags&ntlmKeyExch != 0)
	security.in, security.out = security.out, security.in
	return p, security
}

func TestNTLMv2ResponseKeyVector(t *testing.T) {
	// MS-NLMP 4.2.4.1.1 NTOWFv2 example.
	if got := hex.EncodeToString(ntlmResponseKey("User", "Domain", "Password")); got != "0c868a403bfd7a93a3001ef22ef02e3f" {
		t.Fatalf("NTOWFv2=%s", got)
	}
}

func TestNTLMv2Authentication(t *testing.T) {
	for _, flags := range []uint32{ntlmRequired, ntlmRequired | ntlmVersion | ntlmKeyExch} {
		negotiate := testNLANegotiate(flags)
		a, err := newNTLMAcceptor(negotiate)
		if err != nil {
			t.Fatal(err)
		}
		auth, peer := testNLAAuthenticate(t, negotiate, a.challenge, "anykvm", "test-password")
		security, err := a.authenticate(auth, "anykvm", "test-password")
		if err != nil {
			t.Fatal(err)
		}
		for _, plaintext := range [][]byte{[]byte("public key binding"), []byte("delegated credentials")} {
			got, err := security.unseal(peer.seal(plaintext))
			if err != nil || !bytes.Equal(got, plaintext) {
				t.Fatalf("client sealing failed: %v", err)
			}
			got, err = peer.unseal(security.seal(plaintext))
			if err != nil || !bytes.Equal(got, plaintext) {
				t.Fatalf("server sealing failed: %v", err)
			}
		}
		if _, err := a.authenticate(auth, "anykvm", "wrong-password"); err == nil {
			t.Fatal("accepted wrong password")
		}
		if _, err := a.authenticate(auth, "different-user", "test-password"); err == nil {
			t.Fatal("accepted wrong username")
		}
		for n := range auth {
			if _, err := a.authenticate(auth[:n], "anykvm", "test-password"); err == nil {
				t.Fatalf("accepted truncated NTLM Authenticate at %d", n)
			}
		}
		bad := append([]byte(nil), auth...)
		micOff := 64
		if flags&ntlmVersion != 0 {
			micOff += 8
		}
		bad[micOff] ^= 1
		if _, err := a.authenticate(bad, "anykvm", "test-password"); err == nil {
			t.Fatal("accepted corrupted MIC")
		}
		bad = append([]byte(nil), auth...)
		le.PutUint32(bad[24:], 0xffffffff)
		if _, err := a.authenticate(bad, "anykvm", "test-password"); err == nil {
			t.Fatal("accepted out-of-bounds NTLM security buffer")
		}
	}
}

func TestCredSSPFraming(t *testing.T) {
	req := tsRequest{Version: 6, NegoTokens: []negoToken{{Token: bytes.Repeat([]byte{1}, 512)}}}
	p, err := asn1.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	reader := bytes.NewReader(join(p, []byte("MCS")))
	got, err := readTSRequest(reader)
	if err != nil || got.Version != 6 || !bytes.Equal(got.NegoTokens[0].Token, req.NegoTokens[0].Token) || reader.Len() != 3 {
		t.Fatalf("CredSSP framing or read-ahead: %v", err)
	}
	for n := range p {
		if _, err := readTSRequest(bytes.NewReader(p[:n])); err == nil {
			t.Fatalf("accepted truncated CredSSP at %d", n)
		}
	}
	for _, bad := range [][]byte{{0x30, 0x80}, {0x30, 0x83, 1, 0, 0}, {0x30, 0x82, 0xff, 0xff}, {0x31, 0}} {
		if _, err := readTSRequest(bytes.NewReader(bad)); err == nil {
			t.Fatalf("accepted invalid DER header %x", bad)
		}
	}
}

func testNLAConnection(t *testing.T, protocol uint32, cfg *tls.Config) (*tls.Conn, *Server, <-chan error) {
	t.Helper()
	if cfg == nil {
		cfg = &tls.Config{Certificates: []tls.Certificate{testNLACertificate(t)}}
	}
	raw, peer := net.Pipe()
	s := &Server{cfg: Config{Username: "anykvm", Password: "test-password", TLSConfig: cfg}, clients: make(map[*client]bool)}
	c := &client{s: s, raw: raw, done: make(chan struct{})}
	c.conn = tls.Server(raw, nlaTLSConfig(cfg, &c.tlsCertificate))
	result := make(chan error, 1)
	go func() {
		result <- c.run()
		raw.Close()
	}()
	t.Cleanup(func() { peer.Close(); raw.Close() })
	peer.SetDeadline(time.Now().Add(5 * time.Second))
	request := []byte{14, 0xe0, 0, 0, 0, 0, 0, 1, 0, 8, 0, 0, 0, 0, 0}
	le.PutUint32(request[11:], protocol)
	if _, err := peer.Write(tpkt(request)); err != nil {
		t.Fatal(err)
	}
	response, fast, err := readPacket(peer)
	if err != nil || fast || len(response) != 15 || response[7] != 2 || le.Uint32(response[11:]) != selectSecurityProtocol(protocol) {
		t.Fatalf("negotiation=%x, error=%v", response, err)
	}
	conn := tls.Client(peer, &tls.Config{InsecureSkipVerify: true})
	if err := conn.Handshake(); err != nil {
		t.Fatal(err)
	}
	return conn, s, result
}

func testSendTS(t *testing.T, conn io.Writer, req tsRequest) {
	t.Helper()
	if req.Version == 0 {
		req.Version = 6
	}
	p, err := asn1.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(p); err != nil {
		t.Fatal(err)
	}
}

func testMCSClientInfo(t *testing.T, conn *tls.Conn, user, pass string, accepted bool) {
	t.Helper()
	write := func(p []byte) {
		t.Helper()
		if _, err := conn.Write(tpkt(p)); err != nil {
			t.Fatal(err)
		}
	}
	read := func() []byte {
		t.Helper()
		p, fast, err := readPacket(conn)
		if err != nil || fast {
			t.Fatalf("MCS response: %v", err)
		}
		return p
	}
	core := make([]byte, 142)
	le.PutUint16(core[4:], 640)
	le.PutUint16(core[6:], 480)
	le.PutUint16(core[140:], 0x100)
	write(dpiConnectPacket(core))
	read()
	write([]byte{2, 0xf0, 0x80, 0x28})
	read()
	write([]byte{2, 0xf0, 0x80, 0x38, 0, 1, 3, 0xeb})
	read()
	payload := testInfo(user, pass)
	write(join([]byte{2, 0xf0, 0x80, 0x64, 0, 1, 3, 0xeb, 0x70}, perLength(len(payload)), payload))
	if accepted {
		license := read()
		if !bytes.Contains(license, []byte{0x80, 0, 0, 0, 0xff, 3, 16, 0}) {
			t.Fatalf("expected licensing completion, got %x", license)
		}
		read() // Demand Active is only sent after authentication.
	} else if _, _, err := readPacket(conn); err == nil {
		t.Fatal("unauthenticated Client Info received an activation response")
	}
}

func TestNLAConnection(t *testing.T) {
	for _, mode := range []string{"hybrid", "hybrid-ex", "spnego", "version-5", "certificate-callback", "bad-password", "bad-binding", "bad-signature", "bad-spnego-mic", "bad-delegated-password", "old-credssp"} {
		t.Run(mode, func(t *testing.T) {
			protocol := protocolTLS | protocolHybrid
			if mode == "hybrid-ex" {
				protocol |= protocolHybridEx
			}
			var cfg *tls.Config
			if mode == "certificate-callback" {
				first, selected := testNLACertificate(t), testNLACertificate(t)
				cfg = &tls.Config{Certificates: []tls.Certificate{first}, GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &selected, nil }}
			}
			conn, server, result := testNLAConnection(t, protocol, cfg)
			version := 6
			if mode == "version-5" {
				version = 5
			}
			if mode == "old-credssp" {
				version = 4
			}
			negotiate := testNLANegotiate(ntlmRequired | ntlmVersion | ntlmKeyExch)
			wrapped := mode == "spnego" || mode == "bad-spnego-mic"
			negotiateToken := negotiate
			mechanisms, _ := asn1.Marshal([]asn1.ObjectIdentifier{ntlmOID})
			if wrapped {
				oid, _ := asn1.Marshal(spnegoOID)
				token, _ := asn1.Marshal(negotiate)
				init := join(berValue([]byte{0xa0}, mechanisms), berValue([]byte{0xa2}, token))
				negotiateToken = berValue([]byte{0x60}, join(oid, berValue([]byte{0xa0}, berValue([]byte{0x30}, init))))
			}
			testSendTS(t, conn, tsRequest{Version: version, NegoTokens: []negoToken{{Token: negotiateToken}}})
			response, err := readTSRequest(conn)
			if err != nil {
				t.Fatal(err)
			}
			failure := func(response tsRequest, err error) {
				t.Helper()
				if err != nil || response.ErrorCode != 0xc000006d {
					t.Fatalf("missing logon failure status: %+v %v", response, err)
				}
				if err := <-result; err == nil {
					t.Fatal("accepted invalid NLA credentials")
				}
				if server.active != nil {
					t.Fatal("failed NLA login took over the active session")
				}
			}
			if mode == "old-credssp" {
				failure(response, err)
				return
			}
			password := "test-password"
			if mode == "bad-password" {
				password = "wrong-password"
			}
			challenge, _, err := unwrapNTLM(response.NegoTokens[0].Token)
			if err != nil {
				t.Fatal(err)
			}
			auth, security := testNLAAuthenticate(t, negotiate, challenge, "anykvm", password)
			if wrapped {
				mic := security.mechMIC(mechanisms)
				if mode == "bad-spnego-mic" {
					mic[5] ^= 1
				}
				auth = wrapNTLM(auth, false, mic)
			}
			publicKey, err := credSSPPublicKey(&tls.Certificate{Certificate: [][]byte{conn.ConnectionState().PeerCertificates[0].Raw}})
			if err != nil {
				t.Fatal(err)
			}
			nonce := bytes.Repeat([]byte{0x21}, 32)
			binding := credSSPBinding(publicKey, nonce, "Client-To-Server")
			if mode == "bad-binding" {
				binding[0] ^= 1
			}
			sealed := security.seal(binding)
			if mode == "bad-signature" {
				sealed[5] ^= 1
			}
			testSendTS(t, conn, tsRequest{Version: version, NegoTokens: []negoToken{{Token: auth}}, PubKeyAuth: sealed, ClientNonce: nonce})
			response, err = readTSRequest(conn)
			if mode == "bad-password" || mode == "bad-binding" || mode == "bad-signature" || mode == "bad-spnego-mic" {
				failure(response, err)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if wrapped {
				var outer asn1.RawValue
				var reply struct {
					State asn1.Enumerated `asn1:"explicit,tag:0"`
					MIC   []byte          `asn1:"explicit,tag:3"`
				}
				_, err := asn1.Unmarshal(response.NegoTokens[0].Token, &outer)
				if err == nil {
					_, err = asn1.Unmarshal(outer.Bytes, &reply)
				}
				if err != nil || reply.State != 0 || security.verifyMIC(mechanisms, reply.MIC) != nil {
					t.Fatalf("server SPNEGO MIC: %v", err)
				}
			}
			proof, err := security.unseal(response.PubKeyAuth)
			if err != nil || !bytes.Equal(proof, credSSPBinding(publicKey, nonce, "Server-To-Client")) {
				t.Fatalf("server public-key proof: %v", err)
			}
			if mode == "bad-delegated-password" {
				password = "wrong-delegated-password"
			}
			creds, _ := asn1.Marshal(tsPasswordCredentials{Domain: utf16Bytes("ANYKVM"), Username: utf16Bytes("AnyKVM"), Password: utf16Bytes(password)})
			creds, _ = asn1.Marshal(tsCredentials{Type: 1, Credentials: creds})
			testSendTS(t, conn, tsRequest{Version: version, AuthInfo: security.seal(creds)})
			if mode == "bad-delegated-password" {
				response, err = readTSRequest(conn)
				failure(response, err)
				return
			}
			if mode == "hybrid-ex" {
				var result [4]byte
				if _, err := io.ReadFull(conn, result[:]); err != nil || le.Uint32(result[:]) != 0 {
					t.Fatalf("early authorization result: %x %v", result, err)
				}
			}
			// Native NLA clients need not repeat credentials in Client Info.
			testMCSClientInfo(t, conn, "", "", true)
			testAuthenticatedLogon(t, conn, "ANYKVM", "AnyKVM")
		})
	}
}

func TestTLSClientInfoAuthenticationRetained(t *testing.T) {
	for _, pass := range []string{"test-password", "", "wrong-password"} {
		t.Run("password="+pass, func(t *testing.T) {
			conn, _, _ := testNLAConnection(t, protocolTLS, nil)
			testMCSClientInfo(t, conn, "anykvm", pass, pass == "test-password")
			if pass == "test-password" {
				testAuthenticatedLogon(t, conn, "", "anykvm")
			}
		})
	}
}

func TestSecurityProtocolSelection(t *testing.T) {
	for _, tc := range []struct{ requested, want uint32 }{{0, 0}, {4, 0}, {1, 1}, {2, 2}, {3, 2}, {8, 8}, {11, 8}} {
		if got := selectSecurityProtocol(tc.requested); got != tc.want {
			t.Fatalf("requested=%#x selected=%#x want=%#x", tc.requested, got, tc.want)
		}
	}
}

func FuzzCredSSPFraming(f *testing.F) {
	p, _ := asn1.Marshal(tsRequest{Version: 6, NegoTokens: []negoToken{{Token: testNLANegotiate(ntlmRequired)}}})
	f.Add(p)
	f.Add([]byte{0x30, 0x82, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, p []byte) {
		readTSRequest(bytes.NewReader(p))
		unwrapNTLM(p)
	})
}

func FuzzNTLMAuthenticate(f *testing.F) {
	a, err := newNTLMAcceptor(testNLANegotiate(ntlmRequired | ntlmVersion | ntlmKeyExch))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(testNLANegotiate(ntlmRequired))
	f.Add(join(ntlmSignature, u32(3), make([]byte, 76)))
	f.Fuzz(func(t *testing.T, p []byte) {
		newNTLMAcceptor(p)
		a.authenticate(p, "anykvm", "test-password")
	})
}
