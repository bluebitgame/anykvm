package tlsconfig

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	defaultCertName = "cert.pem"
	defaultKeyName  = "key.pem"
)

// EnsureDefault returns the paths of the default TLS cert/key stored next
// to the executable. When they do not exist yet, a self-signed pair covering
// localhost and all local interface addresses is generated and saved so the
// same certificate is reused across restarts.
func EnsureDefault() (certFile, keyFile string, generated bool, err error) {
	dir := "."
	if exe, exeErr := os.Executable(); exeErr == nil {
		dir = filepath.Dir(exe)
	}
	certFile = filepath.Join(dir, defaultCertName)
	keyFile = filepath.Join(dir, defaultKeyName)

	if fileExists(certFile) && fileExists(keyFile) {
		return certFile, keyFile, false, nil
	}

	certPEM, keyPEM, err := generateSelfSignedPEM()
	if err != nil {
		return "", "", false, err
	}
	if err = os.WriteFile(certFile, certPEM, 0o644); err != nil {
		return "", "", false, err
	}
	if err = os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		return "", "", false, err
	}
	return certFile, keyFile, true, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// generateSelfSignedPEM creates an RSA-2048 self-signed certificate valid for
// 5 years, covering localhost, loopback and every non-loopback local IP so
// both RDP clients and browsers can connect by LAN address.
func generateSelfSignedPEM() (certPEM, keyPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}

	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "AnyKVM"},
		NotBefore:             time.Now().Add(-24 * time.Hour),
		NotAfter:              time.Now().AddDate(5, 0, 0),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost", "AnyKVM"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	tmpl.IPAddresses = append(tmpl.IPAddresses, localIPs()...)

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM, nil
}

// localIPs returns the host's non-loopback interface addresses.
func localIPs() []net.IP {
	var ips []net.IP
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ips
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() {
			ips = append(ips, ipn.IP)
		}
	}
	return ips
}
