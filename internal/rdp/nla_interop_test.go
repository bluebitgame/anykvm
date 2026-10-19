package rdp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestCredSSPIndependentClient(t *testing.T) {
	python := os.Getenv("ANYKVM_TEST_CREDSSP_PYTHON")
	if python == "" {
		t.Skip("set ANYKVM_TEST_CREDSSP_PYTHON to a Python interpreter with pyspnego installed")
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
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
	s, err := NewServer(Config{Addr: "127.0.0.1:0", Username: "anykvm", Password: "test-password",
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, mode := range []string{"raw", "hybrid-ex", "spnego", "bad-password"} {
		t.Run(mode, func(t *testing.T) {
			// Reconnect using the same credentials with a fresh independent
			// authentication context; this does not test native client storage.
			for attempt := 1; attempt <= 2; attempt++ {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				command := exec.CommandContext(ctx, python, "testdata/credssp_client.py", s.Addr().String(), mode)
				output, err := command.CombinedOutput()
				cancel()
				if err != nil {
					t.Fatalf("independent CredSSP client (attempt %d): %v\n%s", attempt, err, output)
				}
				t.Logf("attempt %d: %s", attempt, output)
			}
		})
	}
}
