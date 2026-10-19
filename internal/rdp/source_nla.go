package rdp

import (
	"bytes"
	"crypto/rand"
	"crypto/rc4"
	"crypto/tls"
	"encoding/asn1"
	"fmt"
	"time"
)

func sourceNTLM(negotiate, challenge []byte, user, domain, password string) ([]byte, *ntlmSecurity, error) {
	if len(challenge) < 48 || !bytes.Equal(challenge[:8], ntlmSignature) || le.Uint32(challenge[8:]) != 2 {
		return nil, nil, errProtocol
	}
	flags := le.Uint32(challenge[20:])
	if flags&ntlmRequired != ntlmRequired {
		return nil, nil, fmt.Errorf("RDP NLA requires NTLMv2 signing and sealing")
	}
	flags &= ntlmRequired | ntlmVersion | ntlmKeyExch | 0x00820004
	info, err := ntlmField(challenge, 40, 48)
	if err != nil {
		return nil, nil, err
	}
	var avs []byte
	timestamp := make([]byte, 8)
	le.PutUint64(timestamp, uint64(time.Now().UnixNano()/100)+116444736000000000)
	hasStamp, hasFlags, ended := false, false, false
	for p := info; len(p) >= 4; {
		id, n := le.Uint16(p), int(le.Uint16(p[2:]))
		p = p[4:]
		if n > len(p) {
			return nil, nil, errProtocol
		}
		if id == 0 {
			if n != 0 || len(p) != 0 {
				return nil, nil, errProtocol
			}
			ended = true
			break
		}
		v := p[:n]
		p = p[n:]
		if id == 7 {
			if n != 8 {
				return nil, nil, errProtocol
			}
			copy(timestamp, v)
			hasStamp = true
		}
		if id == 6 {
			if n != 4 {
				return nil, nil, errProtocol
			}
			v = u32(le.Uint32(v) | 2)
			hasFlags = true
		}
		avs = append(avs, join(u16(int(id)), u16(len(v)), v)...)
	}
	if !ended {
		return nil, nil, errProtocol
	}
	if !hasStamp {
		avs = append(avs, join(u16(7), u16(8), timestamp)...)
	}
	if !hasFlags {
		avs = append(avs, join(u16(6), u16(4), u32(2))...)
	}
	avs = append(avs, 0, 0, 0, 0)
	nonce := make([]byte, 8)
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	blob := join([]byte{1, 1, 0, 0, 0, 0, 0, 0}, timestamp, nonce, make([]byte, 4), avs, make([]byte, 4))
	key := ntlmResponseKey(user, domain, password)
	proof := hmacMD5(key, challenge[24:32], blob)
	key = hmacMD5(key, proof)
	var encrypted []byte
	if flags&ntlmKeyExch != 0 {
		exported := make([]byte, 16)
		if _, err = rand.Read(exported); err != nil {
			return nil, nil, err
		}
		cipher, _ := rc4.NewCipher(key)
		encrypted = make([]byte, 16)
		cipher.XORKeyStream(encrypted, exported)
		key = exported
	}
	micOff := 64
	if flags&ntlmVersion != 0 {
		micOff = 72
	}
	p := make([]byte, micOff+16)
	copy(p, ntlmSignature)
	le.PutUint32(p[8:], 3)
	le.PutUint32(p[60:], flags)
	if micOff == 72 {
		copy(p[64:], []byte{6, 1, 0, 0, 0, 0, 0, 15})
	}
	for i, v := range [][]byte{make([]byte, 24), join(proof, blob), utf16Bytes(domain), utf16Bytes(user), nil, encrypted} {
		putNTLMField(p, 12+i*8, len(p), len(v))
		p = append(p, v...)
	}
	copy(p[micOff:], hmacMD5(key, negotiate, challenge, p))
	security := newNTLMSecurity(key, flags&ntlmKeyExch != 0)
	security.in, security.out = security.out, security.in
	return p, security, nil
}
func (s *sourceSession) sendTS(req tsRequest) error {
	req.Version = credSSPVersion
	p, e := asn1.Marshal(req)
	if e != nil {
		return e
	}
	return s.write(p)
}
func (s *sourceSession) sourceNLA(conn *tls.Conn, password string) error {
	negotiate := make([]byte, 40)
	copy(negotiate, ntlmSignature)
	le.PutUint32(negotiate[8:], 1)
	le.PutUint32(negotiate[12:], ntlmRequired|ntlmVersion|ntlmKeyExch|0x00800000)
	copy(negotiate[32:], []byte{6, 1, 0, 0, 0, 0, 0, 15})
	if err := s.sendTS(tsRequest{NegoTokens: []negoToken{{Token: negotiate}}}); err != nil {
		return err
	}
	req, err := readTSRequest(conn)
	if err != nil {
		return err
	}
	if req.Version < 5 || req.ErrorCode != 0 || len(req.NegoTokens) != 1 {
		return errNLAAuth
	}
	challenge, wrapped, err := unwrapNTLM(req.NegoTokens[0].Token)
	if err != nil {
		return err
	}
	auth, security, err := sourceNTLM(negotiate, challenge, s.opts.Username, s.opts.Domain, password)
	if err != nil {
		return err
	}
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return errNLAAuth
	}
	pub, err := credSSPPublicKey(&tls.Certificate{Certificate: [][]byte{certs[0].Raw}})
	if err != nil {
		return err
	}
	nonce := make([]byte, 32)
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	if wrapped {
		auth = wrapNTLM(auth, false)
	}
	if err = s.sendTS(tsRequest{NegoTokens: []negoToken{{Token: auth}}, ClientNonce: nonce, PubKeyAuth: security.seal(credSSPBinding(pub, nonce, "Client-To-Server"))}); err != nil {
		return err
	}
	req, err = readTSRequest(conn)
	if err != nil {
		return err
	}
	if req.Version < 5 || req.ErrorCode != 0 || len(req.PubKeyAuth) == 0 {
		return errNLAAuth
	}
	binding, err := security.unseal(req.PubKeyAuth)
	if err != nil || !bytes.Equal(binding, credSSPBinding(pub, nonce, "Server-To-Client")) {
		return fmt.Errorf("RDP NLA server public-key binding failed")
	}
	creds, err := asn1.Marshal(tsPasswordCredentials{Domain: utf16Bytes(s.opts.Domain), Username: utf16Bytes(s.opts.Username), Password: utf16Bytes(password)})
	if err != nil {
		return err
	}
	creds, err = asn1.Marshal(tsCredentials{Type: 1, Credentials: creds})
	if err != nil {
		return err
	}
	return s.sendTS(tsRequest{AuthInfo: security.seal(creds)})
}
