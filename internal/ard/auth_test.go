package ard

import (
	"bytes"
	"crypto/aes"
	"crypto/md5"
	"fmt"
)

func verifyDHCredentials(secret, encrypted []byte) error {
	key := md5.Sum(secret)
	b, err := aes.NewCipher(key[:])
	if err != nil {
		return err
	}
	p := bytes.Clone(encrypted)
	for i := 0; i < len(p); i += 16 {
		b.Decrypt(p[i:i+16], p[i:i+16])
	}
	if string(bytes.SplitN(p[:64], []byte{0}, 2)[0]) != "tester" || string(bytes.SplitN(p[64:], []byte{0}, 2)[0]) != "test-password" {
		return fmt.Errorf("DH credential cipher/order mismatch")
	}
	return nil
}
