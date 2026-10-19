package vnc

import (
	"crypto/des"
	"crypto/rand"
	"io"
)

// generateChallenge creates a 16-byte random challenge.
func generateChallenge() ([]byte, error) {
	challenge := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, challenge); err != nil {
		return nil, err
	}
	return challenge, nil
}

// vncEncrypt encrypts a 16-byte challenge with the VNC DES key derived from password.
func vncEncrypt(challenge []byte, password string) ([]byte, error) {
	key := makeVNCKey(password)

	block, err := des.NewCipher(key)
	if err != nil {
		return nil, err
	}

	encrypted := make([]byte, 16)
	block.Encrypt(encrypted[0:8], challenge[0:8])
	block.Encrypt(encrypted[8:16], challenge[8:16])

	return encrypted, nil
}

// makeVNCKey creates a DES key (8 bytes) from a VNC password.
// The password is truncated or null-padded to 8 bytes,
// then each byte's bits are reversed.
func makeVNCKey(password string) []byte {
	key := make([]byte, 8)
	copy(key, []byte(password))

	for i := range key {
		key[i] = reverseBits(key[i])
	}
	return key
}

// reverseBits reverses the bit order of a byte.
func reverseBits(b byte) byte {
	b = ((b & 0x55) << 1) | ((b & 0xAA) >> 1)
	b = ((b & 0x33) << 2) | ((b & 0xCC) >> 2)
	b = ((b & 0x0F) << 4) | ((b & 0xF0) >> 4)
	return b
}
