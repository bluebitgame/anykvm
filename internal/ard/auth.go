package ard

import (
	"crypto/aes"
	"crypto/md5" // Apple security type 30 specifies MD5 of the shared secret.
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"
)

func credentialBlock(username, password string, key []byte) ([]byte, error) {
	p := make([]byte, 128)
	if _, err := rand.Read(p); err != nil {
		return nil, err
	}
	copy(p, username)
	p[len(username)] = 0
	copy(p[64:], password)
	p[64+len(password)] = 0
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(p); i += aes.BlockSize {
		b.Encrypt(p[i:i+16], p[i:i+16])
	}
	return p, nil
}

func (s *session) authDH(username, password string) error {
	h, err := s.read(4)
	if err != nil {
		return err
	}
	g, n := int(be16(h)), int(be16(h[2:]))
	if n < 16 || n > 512 || g < 2 {
		return fmt.Errorf("ard: invalid Apple DH parameters")
	}
	p, err := s.read(n * 2)
	if err != nil {
		return err
	}
	prime, pub := new(big.Int).SetBytes(p[:n]), new(big.Int).SetBytes(p[n:])
	if !prime.ProbablyPrime(20) || pub.Cmp(big.NewInt(2)) < 0 || pub.Cmp(new(big.Int).Sub(prime, big.NewInt(1))) >= 0 || big.NewInt(int64(g)).Cmp(prime) >= 0 {
		return fmt.Errorf("ard: invalid Apple DH group or public key")
	}
	priv, err := rand.Int(rand.Reader, new(big.Int).Sub(prime, big.NewInt(3)))
	if err != nil {
		return err
	}
	priv.Add(priv, big.NewInt(2))
	shared := new(big.Int).Exp(pub, priv, prime)
	if shared.Cmp(big.NewInt(1)) <= 0 {
		return fmt.Errorf("ard: degenerate Apple DH secret")
	}
	// Apple uses the minimal big-endian integer serialization, without leading zeros.
	key := md5.Sum(shared.Bytes())
	creds, err := credentialBlock(username, password, key[:])
	if err != nil {
		return err
	}
	clientPub := new(big.Int).Exp(big.NewInt(int64(g)), priv, prime).FillBytes(make([]byte, n))
	s.key = append([]byte(nil), key[:]...)
	return s.write(append(creds, clientPub...))
}

func (s *session) authMac(username, password string) error {
	// The type byte and RSA1 capability request are sent in a single write.
	if err := s.write([]byte{33, 0, 0, 0, 10, 1, 0, 'R', 'S', 'A', '1', 0, 0, 0, 0}); err != nil {
		return err
	}
	h, err := s.read(4)
	if err != nil {
		return err
	}
	n := int(be32(h))
	if n < 7 || n > 8192 {
		return fmt.Errorf("ard: invalid RSA capability length")
	}
	p, err := s.read(n)
	if err != nil {
		return err
	}
	klen := int(be32(p[2:6]))
	if klen < 1 || klen > len(p)-6 {
		return fmt.Errorf("ard: invalid RSA public key length")
	}
	pub, err := x509.ParsePKCS1PublicKey(p[6 : 6+klen])
	if err != nil {
		if parsed, e := x509.ParsePKIXPublicKey(p[6 : 6+klen]); e == nil {
			pub, _ = parsed.(*rsa.PublicKey)
		}
	}
	if pub == nil || pub.N.BitLen() < 2048 || pub.N.BitLen() > 4096 {
		return fmt.Errorf("ard: unsupported RSA public key")
	}
	key := make([]byte, 16)
	if _, err = rand.Read(key); err != nil {
		return err
	}
	encryptedKey, err := rsa.EncryptPKCS1v15(rand.Reader, pub, key)
	if err != nil {
		return err
	}
	creds, err := credentialBlock(username, password, key)
	if err != nil {
		return err
	}
	body := append([]byte{1, 0, 'R', 'S', 'A', '1', 0, 1}, creds...)
	body = binary.LittleEndian.AppendUint16(body, uint16(len(encryptedKey)))
	body = append(body, encryptedKey...)
	if err := s.write(binary.BigEndian.AppendUint32(nil, uint32(len(body)))); err != nil {
		return err
	}
	if err := s.write(body); err != nil {
		return err
	}
	if err := s.authResult(); err != nil {
		return err
	}
	s.key = key
	return nil
}

func (s *session) authResult() error {
	p, err := s.read(4)
	if err != nil {
		return err
	}
	if be32(p) != 0 {
		return fmt.Errorf("ard: Apple account authentication rejected")
	}
	return nil
}

func be16(p []byte) uint16     { return binary.BigEndian.Uint16(p) }
func be32(p []byte) uint32     { return binary.BigEndian.Uint32(p) }
func put16(p []byte, n int)    { binary.BigEndian.PutUint16(p, uint16(n)) }
func put32(p []byte, n uint32) { binary.BigEndian.PutUint32(p, n) }
func readExactly(r io.Reader, n int) ([]byte, error) {
	if n < 0 || n > maxPayload {
		return nil, fmt.Errorf("ard: payload exceeds limit")
	}
	p := make([]byte, n)
	_, err := io.ReadFull(r, p)
	return p, err
}
