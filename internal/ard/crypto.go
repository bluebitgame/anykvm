package ard

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
)

// controlCipher implements Apple's ComCryption: continuous CBC in each
// direction, a uint16 payload length, and SHA1(packet counter || padded body).
// Receive operations belong to the control worker; send operations hold writeMu.
type controlCipher struct {
	send, recv     cipher.BlockMode
	sendID, recvID uint32
	synced         bool
}

func newControlCipher(key, iv []byte) (*controlCipher, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(iv) != 16 {
		return nil, fmt.Errorf("ard: invalid control IV")
	}
	return &controlCipher{send: cipher.NewCBCEncrypter(b, iv), recv: cipher.NewCBCDecrypter(b, iv)}, nil
}

func controlDigest(id uint32, p []byte) []byte {
	h := sha1.New()
	h.Write(binary.BigEndian.AppendUint32(nil, id))
	h.Write(p)
	return h.Sum(nil)
}
func (c *controlCipher) encrypt(p []byte) ([]byte, error) {
	n := (2 + len(p) + 20 + 15) &^ 15
	if n > 65535 {
		return nil, fmt.Errorf("ard: control message too large")
	}
	b := make([]byte, n)
	put16(b, len(p))
	copy(b[2:], p)
	copy(b[n-20:], controlDigest(c.sendID, b[:n-20]))
	c.sendID++
	c.send.CryptBlocks(b, b)
	return b, nil
}
func (c *controlCipher) decrypt(b []byte) ([]byte, error) {
	if len(b) < 32 || len(b)%16 != 0 {
		return nil, fmt.Errorf("ard: invalid encrypted control length")
	}
	p := make([]byte, len(b))
	c.recv.CryptBlocks(p, b)
	id, matched := c.recvID, false
	if hmac.Equal(controlDigest(id, p[:len(p)-20]), p[len(p)-20:]) {
		matched = true
	}
	if !matched && !c.synced {
		for i := uint32(0); i < 8; i++ {
			if hmac.Equal(controlDigest(i, p[:len(p)-20]), p[len(p)-20:]) {
				id, matched = i, true
				break
			}
		}
	}
	if !matched {
		return nil, fmt.Errorf("ard: control checksum mismatch")
	}
	n := int(be16(p))
	if n > len(p)-22 {
		return nil, fmt.Errorf("ard: invalid decrypted control length")
	}
	c.synced = true
	c.recvID = id + 1
	return p[2 : 2+n], nil
}

// Older Apple keyed sessions use a fresh ECB-wrapped IV and PKCS7 CBC payload.
func legacyEncrypt(key, p []byte) ([]byte, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	iv := make([]byte, 16)
	if _, err = rand.Read(iv); err != nil {
		return nil, err
	}
	pad := 16 - len(p)%16
	out := make([]byte, 16+len(p)+pad)
	b.Encrypt(out[:16], iv)
	copy(out[16:], p)
	for i := 16 + len(p); i < len(out); i++ {
		out[i] = byte(pad)
	}
	cipher.NewCBCEncrypter(b, iv).CryptBlocks(out[16:], out[16:])
	return out, nil
}
func legacyDecrypt(key, p []byte) ([]byte, error) {
	if len(p) < 32 || len(p)%16 != 0 {
		return nil, fmt.Errorf("ard: invalid legacy control length")
	}
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	iv := make([]byte, 16)
	b.Decrypt(iv, p[:16])
	out := make([]byte, len(p)-16)
	cipher.NewCBCDecrypter(b, iv).CryptBlocks(out, p[16:])
	pad := int(out[len(out)-1])
	if pad < 1 || pad > 16 || pad > len(out) {
		return nil, fmt.Errorf("ard: invalid control padding")
	}
	for _, v := range out[len(out)-pad:] {
		if int(v) != pad {
			return nil, fmt.Errorf("ard: invalid control padding")
		}
	}
	return out[:len(out)-pad], nil
}

type mediaCrypto struct {
	enc        cipher.Block
	auth, salt []byte
}

func newMediaCrypto(key []byte, base byte) (*mediaCrypto, error) {
	if len(key) != 46 {
		return nil, fmt.Errorf("ard: invalid SRTP master material")
	}
	master, err := aes.NewCipher(key[:32])
	if err != nil {
		return nil, err
	}
	derive := func(label byte, n int) []byte {
		iv := make([]byte, 16)
		copy(iv, key[32:])
		iv[7] ^= label
		out := make([]byte, 0, n+16)
		for i := 0; len(out) < n; i++ {
			put16(iv[14:], i)
			p := make([]byte, 16)
			master.Encrypt(p, iv)
			out = append(out, p...)
		}
		return out[:n]
	}
	enc, _ := aes.NewCipher(derive(base, 32))
	return &mediaCrypto{enc: enc, auth: derive(base+1, 20), salt: derive(base+2, 14)}, nil
}
func (c *mediaCrypto) transform(p []byte, ssrc uint32, index uint64) {
	iv := make([]byte, 16)
	copy(iv, c.salt)
	for i := 0; i < 4; i++ {
		iv[4+i] ^= byte(ssrc >> uint(24-8*i))
	}
	for i := 0; i < 6; i++ {
		iv[8+i] ^= byte(index >> uint(40-8*i))
	}
	cipher.NewCTR(c.enc, iv).XORKeyStream(p, p)
}

// Each receive context accepts one video SSRC and tracks a 64-packet replay
// window. An authenticated SSRC change requires fresh negotiation/decoder state.
type srtpReceiver struct {
	*mediaCrypto
	initialized bool
	ssrc        uint32
	highest     uint64
	replay      uint64
}

func (c *srtpReceiver) unprotect(p []byte) ([]byte, error) {
	if len(p) < 22 {
		return nil, fmt.Errorf("ard: truncated SRTP")
	}
	h, err := rtpHeaderLen(p[:len(p)-10])
	if err != nil {
		return nil, err
	}
	seq, ssrc := be16(p[2:]), be32(p[8:])
	index := uint64(seq)
	if c.initialized {
		if ssrc != c.ssrc {
			return nil, fmt.Errorf("ard: unexpected video SSRC")
		}
		roc := c.highest >> 16
		prev := int64(uint16(c.highest))
		cur := int64(seq)
		if prev < 32768 && cur-prev > 32768 && roc > 0 {
			roc--
		} else if prev >= 32768 && prev-cur > 32768 {
			roc++
		}
		index = roc<<16 | uint64(seq)
		if index <= c.highest && (c.highest-index >= 64 || c.replay&(uint64(1)<<(c.highest-index)) != 0) {
			return nil, fmt.Errorf("ard: SRTP replay")
		}
	}
	mac := hmac.New(sha1.New, c.auth)
	mac.Write(p[:len(p)-10])
	mac.Write(binary.BigEndian.AppendUint32(nil, uint32(index>>16)))
	if !hmac.Equal(mac.Sum(nil)[:10], p[len(p)-10:]) {
		return nil, fmt.Errorf("ard: SRTP authentication failed")
	}
	out := append([]byte(nil), p[:len(p)-10]...)
	c.transform(out[h:], ssrc, index)
	if !c.initialized {
		c.ssrc = ssrc
		c.highest = index
		c.replay = 1
		c.initialized = true
	} else if index > c.highest {
		d := index - c.highest
		if d >= 64 {
			c.replay = 1
		} else {
			c.replay = c.replay<<d | 1
		}
		c.highest = index
	} else {
		c.replay |= uint64(1) << (c.highest - index)
	}
	return out, nil
}

type srtcpSender struct {
	*mediaCrypto
	index uint32
}

func (c *srtcpSender) protect(p []byte) []byte {
	c.index = (c.index + 1) & 0x7fffffff
	out := append([]byte(nil), p...)
	c.transform(out[8:], be32(p[4:]), uint64(c.index))
	out = binary.BigEndian.AppendUint32(out, c.index|0x80000000)
	mac := hmac.New(sha1.New, c.auth)
	mac.Write(out)
	return append(out, mac.Sum(nil)[:10]...)
}
