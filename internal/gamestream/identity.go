package gamestream

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// identity is kept separately from Web/VNC TLS: GameStream pins the host's
// certificate, so regenerating it would invalidate every client pairing.
type identity struct {
	Version     int               `json:"version"`
	ID          string            `json:"id"`
	Certificate string            `json:"certificate"`
	PrivateKey  string            `json:"private_key"`
	Clients     map[string]string `json:"clients"` // SHA-256 of DER -> client PEM
}

func fingerprint(cert *x509.Certificate) string {
	h := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(h[:])
}

func parseCertificate(data []byte) (*x509.Certificate, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("expected one PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok || key.N.BitLen() < 2048 || key.N.BitLen() > 4096 {
		return nil, errors.New("pairing requires an RSA-2048 to RSA-4096 certificate")
	}
	return cert, nil
}

func loadIdentity(path string) (identity, tls.Certificate, error) {
	var state identity
	data, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(data, &state)
	} else if errors.Is(err, os.ErrNotExist) {
		state, err = newIdentity()
		if err == nil {
			err = saveIdentity(path, state)
		}
	}
	if err != nil {
		return state, tls.Certificate{}, err
	}
	if state.Version != 1 || state.ID == "" || state.Clients == nil {
		return state, tls.Certificate{}, errors.New("invalid GameStream identity state")
	}
	cert, err := tls.X509KeyPair([]byte(state.Certificate), []byte(state.PrivateKey))
	if err != nil {
		return state, cert, err
	}
	cert.Leaf, err = parseCertificate([]byte(state.Certificate))
	if err != nil {
		return state, cert, err
	}
	for fp, client := range state.Clients {
		c, err := parseCertificate([]byte(client))
		if err != nil || fingerprint(c) != fp {
			return state, cert, errors.New("invalid paired client in GameStream state")
		}
	}
	return state, cert, nil
}

func newIdentity() (identity, error) {
	s := identity{Version: 1, Clients: make(map[string]string)}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return s, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return s, err
	}
	id := make([]byte, 16)
	if _, err = rand.Read(id); err != nil {
		return s, err
	}
	id[6] = (id[6] & 15) | 64
	id[8] = (id[8] & 63) | 128
	s.ID = fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "AnyKVM GameStream"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return s, err
	}
	s.Certificate = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	s.PrivateKey = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	return s, nil
}

// Write and rename within the same directory; a partial write must never
// destroy the host identity or accidentally authorize a client only in memory.
func saveIdentity(path string, state identity) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".gamestream-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
