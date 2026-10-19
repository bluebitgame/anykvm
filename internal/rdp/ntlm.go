package rdp

// The NLA acceptor supports NTLMv2 with 128-bit extended session security.
// Wire layouts and key derivation follow MS-NLMP; NTLMv1 and anonymous logon
// are deliberately excluded. MD4/MD5/RC4 are required by NTLM, inside TLS.
import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/rc4"
	"errors"
	"strings"
	"time"
	"unicode/utf16"

	"golang.org/x/crypto/md4"
)

const (
	ntlmUnicode  = 0x00000001
	ntlmSign     = 0x00000010
	ntlmSeal     = 0x00000020
	ntlmNTLM     = 0x00000200
	ntlmESS      = 0x00080000
	ntlmVersion  = 0x02000000
	ntlm128      = 0x20000000
	ntlmKeyExch  = 0x40000000
	ntlmRequired = ntlmUnicode | ntlmSign | ntlmSeal | ntlmNTLM | ntlmESS | ntlm128
)

var ntlmSignature = []byte("NTLMSSP\x00")
var errNLAAuth = errors.New("NLA authentication failed")

func utf16Bytes(s string) []byte {
	units := utf16.Encode([]rune(s))
	p := make([]byte, len(units)*2)
	for i, u := range units {
		le.PutUint16(p[i*2:], u)
	}
	return p
}

func utf16String(p []byte) (string, error) {
	if len(p)%2 != 0 || len(p) > 4096 {
		return "", errProtocol
	}
	units := make([]uint16, len(p)/2)
	for i := range units {
		units[i] = le.Uint16(p[i*2:])
	}
	return string(utf16.Decode(units)), nil
}

func hmacMD5(key []byte, parts ...[]byte) []byte {
	h := hmac.New(md5.New, key)
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

func ntlmResponseKey(user, domain, password string) []byte {
	h := md4.New()
	h.Write(utf16Bytes(password))
	return hmacMD5(h.Sum(nil), utf16Bytes(strings.ToUpper(user)+domain))
}

// Security buffers contain an absolute offset, length, and allocated length.
func ntlmField(p []byte, off, minimum int) ([]byte, error) {
	if off < 0 || off+8 > len(p) {
		return nil, errProtocol
	}
	n, allocated := int(le.Uint16(p[off:])), int(le.Uint16(p[off+2:]))
	start := uint64(le.Uint32(p[off+4:]))
	if allocated < n || start > uint64(len(p)) || uint64(n) > uint64(len(p))-start || (n != 0 && start < uint64(minimum)) {
		return nil, errProtocol
	}
	return p[int(start) : int(start)+n], nil
}

func putNTLMField(p []byte, off, start, size int) {
	le.PutUint16(p[off:], uint16(size))
	le.PutUint16(p[off+2:], uint16(size))
	le.PutUint32(p[off+4:], uint32(start))
}

type ntlmAcceptor struct {
	negotiate, challenge []byte
	flags                uint32
	timestamp            []byte
}

func newNTLMAcceptor(negotiate []byte) (*ntlmAcceptor, error) {
	if len(negotiate) < 32 || !bytes.Equal(negotiate[:8], ntlmSignature) || le.Uint32(negotiate[8:]) != 1 {
		return nil, errProtocol
	}
	flags := le.Uint32(negotiate[12:])
	if flags&ntlmRequired != ntlmRequired {
		return nil, errors.New("NLA requires NTLMv2 with 128-bit signing and sealing")
	}
	// Preserve only implemented client flags; add server target information.
	flags = flags&(ntlmRequired|ntlmVersion|ntlmKeyExch|0x80008000) | 0x00820004
	header := 48
	if flags&ntlmVersion != 0 {
		if len(negotiate) < 40 {
			return nil, errProtocol
		}
		header += 8
	}
	a := &ntlmAcceptor{negotiate: append([]byte(nil), negotiate...), flags: flags, timestamp: make([]byte, 8)}
	le.PutUint64(a.timestamp, uint64(time.Now().UnixNano()/100)+116444736000000000)
	target := utf16Bytes("ANYKVM")
	info := join(u16(1), u16(len(target)), target, u16(2), u16(len(target)), target)
	info = join(info, u16(7), u16(8), a.timestamp, u16(6), u16(4), u32(2), make([]byte, 4))
	p := make([]byte, header)
	copy(p, ntlmSignature)
	le.PutUint32(p[8:], 2)
	le.PutUint32(p[20:], flags)
	if _, err := rand.Read(p[24:32]); err != nil {
		return nil, err
	}
	putNTLMField(p, 12, header, len(target))
	putNTLMField(p, 40, header+len(target), len(info))
	if flags&ntlmVersion != 0 {
		copy(p[48:], []byte{6, 1, 0, 0, 0, 0, 0, 15})
	}
	a.challenge = join(p, target, info)
	return a, nil
}

func (a *ntlmAcceptor) authenticate(p []byte, username, password string) (*ntlmSecurity, error) {
	if len(p) < 64 || !bytes.Equal(p[:8], ntlmSignature) || le.Uint32(p[8:]) != 3 {
		return nil, errProtocol
	}
	flags := le.Uint32(p[60:])
	if flags&ntlmRequired != ntlmRequired || flags&(ntlmKeyExch|ntlmVersion) != a.flags&(ntlmKeyExch|ntlmVersion) {
		return nil, errNLAAuth
	}
	micOff := 64
	if flags&ntlmVersion != 0 {
		micOff += 8
	}
	if len(p) < micOff+16 {
		return nil, errProtocol
	}
	var fields [6][]byte
	for i := range fields {
		var err error
		fields[i], err = ntlmField(p, 12+i*8, micOff+16)
		if err != nil {
			return nil, err
		}
	}
	domain, err := utf16String(fields[2])
	if err != nil {
		return nil, err
	}
	user, err := utf16String(fields[3])
	if err != nil || !strings.EqualFold(user, username) {
		return nil, errNLAAuth
	}
	response := fields[1]
	if len(response) < 16+32 {
		return nil, errNLAAuth // Includes NTLMv1 and anonymous responses.
	}
	proof, blob := response[:16], response[16:]
	if !bytes.Equal(blob[:8], []byte{1, 1, 0, 0, 0, 0, 0, 0}) || !bytes.Equal(blob[8:16], a.timestamp) || !bytes.Equal(blob[24:28], make([]byte, 4)) {
		return nil, errNLAAuth
	}
	// A timestamped challenge requires a MIC binding the entire exchange.
	avs := blob[28:]
	micRequired, timestampOK, ended := false, false, false
	seen := make(map[uint16]bool)
	for len(avs) >= 4 {
		id, size := le.Uint16(avs), int(le.Uint16(avs[2:]))
		avs = avs[4:]
		if size > len(avs) || seen[id] {
			return nil, errProtocol
		}
		seen[id] = true
		if id == 0 {
			if size != 0 || (len(avs) != 0 && !bytes.Equal(avs, make([]byte, 4))) {
				return nil, errProtocol
			}
			ended = true
			break
		}
		if id == 6 {
			micRequired = size == 4 && le.Uint32(avs)&2 != 0
		}
		if id == 7 {
			timestampOK = size == 8 && bytes.Equal(avs[:size], a.timestamp)
		}
		avs = avs[size:]
	}
	if !ended || !micRequired || !timestampOK {
		return nil, errNLAAuth
	}
	key := ntlmResponseKey(user, domain, password)
	if !hmac.Equal(proof, hmacMD5(key, a.challenge[24:32], blob)) {
		return nil, errNLAAuth
	}
	key = hmacMD5(key, proof)
	if flags&ntlmKeyExch != 0 {
		if len(fields[5]) != 16 {
			return nil, errProtocol
		}
		cipher, _ := rc4.NewCipher(key)
		exported := make([]byte, 16)
		cipher.XORKeyStream(exported, fields[5])
		key = exported
	} else if len(fields[5]) != 0 {
		return nil, errProtocol
	}
	message := append([]byte(nil), p...)
	clear(message[micOff : micOff+16])
	if !hmac.Equal(p[micOff:micOff+16], hmacMD5(key, a.negotiate, a.challenge, message)) {
		return nil, errNLAAuth
	}
	return newNTLMSecurity(key, flags&ntlmKeyExch != 0), nil
}

type ntlmDirection struct {
	signing []byte
	sealing []byte
	cipher  *rc4.Cipher
	seq     uint32
}

type ntlmSecurity struct {
	in, out ntlmDirection
	keyExch bool
}

func newNTLMSecurity(key []byte, keyExch bool) *ntlmSecurity {
	direction := func(name string) ntlmDirection {
		sign := md5.Sum(join(key, []byte("session key to "+name+" signing key magic constant\x00")))
		seal := md5.Sum(join(key, []byte("session key to "+name+" sealing key magic constant\x00")))
		cipher, _ := rc4.NewCipher(seal[:])
		return ntlmDirection{signing: sign[:], sealing: seal[:], cipher: cipher}
	}
	return &ntlmSecurity{in: direction("client-to-server"), out: direction("server-to-client"), keyExch: keyExch}
}

// SPNEGO signs its DER mechanism list before CredSSP binding. MS-SPNG resets
// the NTLM RC4 stream after this MIC while retaining the sequence number.
func (s *ntlmSecurity) verifyMIC(message, mic []byte) error {
	if !hmac.Equal(mic, s.in.signature(message, s.keyExch)) {
		return errNLAAuth
	}
	s.in.cipher, _ = rc4.NewCipher(s.in.sealing)
	return nil
}

func (s *ntlmSecurity) mechMIC(message []byte) []byte {
	mic := s.out.signature(message, s.keyExch)
	s.out.cipher, _ = rc4.NewCipher(s.out.sealing)
	return mic
}

func (d *ntlmDirection) signature(p []byte, keyExch bool) []byte {
	checksum := hmacMD5(d.signing, u32(d.seq), p)[:8]
	if keyExch {
		d.cipher.XORKeyStream(checksum, checksum)
	}
	signature := join(u32(1), checksum, u32(d.seq))
	d.seq++
	return signature
}

func (s *ntlmSecurity) seal(p []byte) []byte {
	ciphertext := make([]byte, len(p))
	s.out.cipher.XORKeyStream(ciphertext, p)
	return join(s.out.signature(p, s.keyExch), ciphertext)
}

func (s *ntlmSecurity) unseal(p []byte) ([]byte, error) {
	if len(p) < 16 {
		return nil, errProtocol
	}
	plaintext := make([]byte, len(p)-16)
	s.in.cipher.XORKeyStream(plaintext, p[16:])
	if !hmac.Equal(p[:16], s.in.signature(plaintext, s.keyExch)) {
		return nil, errNLAAuth
	}
	return plaintext, nil
}
