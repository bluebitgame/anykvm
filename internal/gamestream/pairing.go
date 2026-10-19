package gamestream

import (
	"context"
	"crypto"
	"crypto/aes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"time"
)

type pairSession struct {
	id, peer                      string
	cert                          *x509.Certificate
	certPEM                       string
	expires                       time.Time
	phase                         int
	cancel                        context.CancelFunc
	key                           []byte
	secret, challenge, clientHash []byte
}

func peerAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// GameStream's pairing protocol mandates AES-128-ECB without padding.
// This helper is only for pairing, never for media or control encryption.
func cryptPairing(key, data []byte, encrypt bool) ([]byte, error) {
	if len(key) != 16 || len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("invalid pairing block length")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	for offset := 0; offset < len(data); offset += aes.BlockSize {
		if encrypt {
			block.Encrypt(out[offset:], data[offset:])
		} else {
			block.Decrypt(out[offset:], data[offset:])
		}
	}
	return out, nil
}

func hashParts(parts ...[]byte) []byte {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

func (s *Server) pair(w http.ResponseWriter, r *http.Request, auth bool) {
	q := r.URL.Query()
	if q.Get("phrase") == "pairchallenge" {
		if !auth {
			reply(w, 401, "Paired HTTPS client required", "paired", "0")
			return
		}
		reply(w, 200, "", "paired", "1")
		return
	}
	id := q.Get("uniqueid")
	if id == "" || len(id) > 128 {
		reply(w, 400, "Invalid uniqueid", "paired", "0")
		return
	}
	if q.Get("phrase") == "getservercert" {
		s.beginPair(w, r, id)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pairing
	if p == nil || p.id != id || p.peer != peerAddress(r) {
		reply(w, 400, "No matching pairing session", "paired", "0")
		return
	}
	fail := func(message string) { p.cancel(); s.pairing = nil; reply(w, 400, message, "paired", "0") }
	if !time.Now().Before(p.expires) {
		fail("Pairing expired")
		return
	}
	// Reject multiple stages, unexpected phrases, and out-of-order requests.
	stage, value, count := "", "", 0
	for _, name := range []string{"clientchallenge", "serverchallengeresp", "clientpairingsecret"} {
		if q.Has(name) {
			stage, value = name, q.Get(name)
			count++
		}
	}
	if count != 1 || q.Has("phrase") {
		fail("Invalid pairing stage")
		return
	}
	data, err := hex.DecodeString(value)
	if err != nil {
		fail("Invalid hex payload")
		return
	}
	switch stage {
	case "clientchallenge":
		if p.phase != 1 || len(data) != 16 {
			fail("Invalid client challenge")
			return
		}
		challenge, _ := cryptPairing(p.key, data, false)
		p.secret, p.challenge = make([]byte, 16), make([]byte, 16)
		if _, err = rand.Read(p.secret); err != nil {
			fail("Random generation failed")
			return
		}
		if _, err = rand.Read(p.challenge); err != nil {
			fail("Random generation failed")
			return
		}
		payload := append(hashParts(challenge, s.cert.Leaf.Signature, p.secret), p.challenge...)
		encrypted, _ := cryptPairing(p.key, payload, true)
		p.phase = 2
		reply(w, 200, "", "paired", "1", "challengeresponse", hex.EncodeToString(encrypted))
	case "serverchallengeresp":
		if p.phase != 2 || len(data) != 32 {
			fail("Invalid server challenge response")
			return
		}
		p.clientHash, _ = cryptPairing(p.key, data, false)
		digest := sha256.Sum256(p.secret)
		signature, err := rsa.SignPKCS1v15(rand.Reader, s.cert.PrivateKey.(*rsa.PrivateKey), crypto.SHA256, digest[:])
		if err != nil {
			fail("Signing failed")
			return
		}
		payload := append(append([]byte{}, p.secret...), signature...)
		p.phase = 3
		reply(w, 200, "", "paired", "1", "pairingsecret", hex.EncodeToString(payload))
	case "clientpairingsecret":
		key := p.cert.PublicKey.(*rsa.PublicKey)
		if p.phase != 3 || len(data) != 16+key.Size() {
			fail("Invalid client pairing secret")
			return
		}
		secret, signature := data[:16], data[16:]
		digest := sha256.Sum256(secret)
		if subtle.ConstantTimeCompare(hashParts(p.challenge, p.cert.Signature, secret), p.clientHash) != 1 ||
			rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) != nil {
			fail("Pairing verification failed")
			return
		}
		// Copy before persisting: a disk failure must not authorize this client.
		next := s.state
		next.Clients = make(map[string]string, len(s.state.Clients)+1)
		for k, v := range s.state.Clients {
			next.Clients[k] = v
		}
		next.Clients[fingerprint(p.cert)] = p.certPEM
		if err := saveIdentity(s.cfg.StateFile, next); err != nil {
			p.cancel()
			s.pairing = nil
			reply(w, 500, "Unable to save paired client", "paired", "0")
			return
		}
		s.state.Clients = next.Clients
		p.cancel()
		s.pairing = nil
		reply(w, 200, "", "paired", "1")
	}
}

func (s *Server) beginPair(w http.ResponseWriter, r *http.Request, id string) {
	if s.cfg.RequestPIN == nil {
		reply(w, 403, "New pairings disabled", "paired", "0")
		return
	}
	q := r.URL.Query()
	salt, err := hex.DecodeString(q.Get("salt"))
	if err != nil || len(salt) != 16 {
		reply(w, 400, "Invalid salt", "paired", "0")
		return
	}
	data, err := hex.DecodeString(q.Get("clientcert"))
	if err != nil {
		reply(w, 400, "Invalid client certificate", "paired", "0")
		return
	}
	cert, err := parseCertificate(data)
	if err != nil {
		reply(w, 400, "Invalid client certificate", "paired", "0")
		return
	}
	for _, name := range []string{"clientchallenge", "serverchallengeresp", "clientpairingsecret"} {
		if q.Has(name) {
			reply(w, 400, "Ambiguous pairing stage", "paired", "0")
			return
		}
	}
	if len(q.Get("devicename")) > 128 {
		reply(w, 400, "Client name too long", "paired", "0")
		return
	}
	p := &pairSession{id: id, peer: peerAddress(r), cert: cert, certPEM: string(data), expires: time.Now().Add(2 * time.Minute)}
	ctx, cancel := context.WithDeadline(r.Context(), p.expires)
	p.cancel = cancel
	defer cancel()
	s.mu.Lock()
	if s.pairing != nil && time.Now().Before(s.pairing.expires) {
		s.mu.Unlock()
		reply(w, 409, "Another pairing is in progress", "paired", "0")
		return
	}
	if s.pairing != nil {
		s.pairing.cancel()
	}
	s.pairing = p
	s.mu.Unlock()
	// No state lock while the operator is entering the client-displayed PIN.
	pin, err := s.cfg.RequestPIN(ctx, PairingRequest{ClientName: q.Get("devicename"), Address: p.peer})
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pairing != p {
		reply(w, 400, "Pairing cancelled", "paired", "0")
		return
	}
	valid := len(pin) == 4
	for _, c := range pin {
		valid = valid && c >= '0' && c <= '9'
	}
	if err != nil || ctx.Err() != nil || !time.Now().Before(p.expires) || !valid {
		s.pairing = nil
		reply(w, 400, "PIN entry cancelled, expired, or invalid", "paired", "0")
		return
	}
	p.key = hashParts(salt, []byte(pin))[:16]
	p.phase = 1
	reply(w, 200, "", "paired", "1", "plaincert", hex.EncodeToString([]byte(s.state.Certificate)))
}

func (s *Server) unpair(w http.ResponseWriter, r *http.Request, auth bool) {
	s.launchMu.Lock()
	defer s.launchMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if auth {
		fp := fingerprint(r.TLS.PeerCertificates[0])
		next := s.state
		next.Clients = make(map[string]string, len(s.state.Clients))
		for k, v := range s.state.Clients {
			if k != fp {
				next.Clients[k] = v
			}
		}
		if err := saveIdentity(s.cfg.StateFile, next); err != nil {
			reply(w, 500, "Unable to save unpairing")
			return
		}
		s.state.Clients = next.Clients
		if x := s.currentSession(); x != nil && x.owner == fp {
			x.stop("client unpaired")
		}
	} else {
		// GameStream uses plaintext /unpair to clean up failed pairing attempts.
		// It may cancel that attempt, but must never revoke a paired certificate.
		if p := s.pairing; p != nil && p.id == r.URL.Query().Get("uniqueid") && p.peer == peerAddress(r) {
			p.cancel()
			s.pairing = nil
		}
	}
	reply(w, 200, "", "paired", "0")
}
