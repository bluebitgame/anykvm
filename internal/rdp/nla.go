package rdp

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
	"io"
	"strings"
)

func selectSecurityProtocol(requested uint32) uint32 {
	for _, protocol := range []uint32{protocolHybridEx, protocolHybrid, protocolTLS} {
		if requested&protocol != 0 {
			return protocol
		}
	}
	return 0
}

// Capture the certificate selected by Go's TLS server for the CredSSP binding,
// including SNI/callback selection. Certificates is cleared in the clone so
// GetCertificate also runs when the client omits SNI (common for IP addresses).
func nlaTLSConfig(cfg *tls.Config, selected **tls.Certificate) *tls.Config {
	clone := cfg.Clone()
	clone.Certificates = nil
	clone.GetCertificate = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if cfg.GetCertificate != nil {
			cert, err := cfg.GetCertificate(hello)
			if cert != nil || err != nil {
				*selected = cert
				return cert, err
			}
		}
		if len(cfg.Certificates) == 0 {
			return nil, errors.New("RDP requires a TLS certificate")
		}
		name := strings.TrimRight(strings.ToLower(hello.ServerName), ".")
		named := cfg.NameToCertificate[name]
		if named == nil {
			if dot := strings.IndexByte(name, '.'); dot >= 0 {
				named = cfg.NameToCertificate["*"+name[dot:]]
			}
		}
		if named != nil {
			*selected = named
			return named, nil
		}
		cert := &cfg.Certificates[0]
		for i := range cfg.Certificates {
			if hello.SupportsCertificate(&cfg.Certificates[i]) == nil {
				cert = &cfg.Certificates[i]
				break
			}
		}
		*selected = cert
		return cert, nil
	}
	if cfg.GetConfigForClient != nil {
		clone.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			next, err := cfg.GetConfigForClient(hello)
			if next == nil || err != nil {
				return next, err
			}
			return nlaTLSConfig(next, selected), nil
		}
	}
	return clone
}

const (
	protocolTLS      uint32 = 1
	protocolHybrid   uint32 = 2
	protocolHybridEx uint32 = 8
	credSSPVersion          = 6
	maxCredSSP              = 65535
)

type negoToken struct {
	Token []byte `asn1:"explicit,tag:0"`
}

type tsRequest struct {
	Version     int         `asn1:"explicit,tag:0"`
	NegoTokens  []negoToken `asn1:"optional,explicit,tag:1"`
	AuthInfo    []byte      `asn1:"optional,explicit,tag:2"`
	PubKeyAuth  []byte      `asn1:"optional,explicit,tag:3"`
	ErrorCode   int64       `asn1:"optional,explicit,tag:4"`
	ClientNonce []byte      `asn1:"optional,explicit,tag:5"`
}

type tsCredentials struct {
	Type        int    `asn1:"explicit,tag:0"`
	Credentials []byte `asn1:"explicit,tag:1"`
}

type tsPasswordCredentials struct {
	Domain   []byte `asn1:"explicit,tag:0"`
	Username []byte `asn1:"explicit,tag:1"`
	Password []byte `asn1:"explicit,tag:2"`
}

func readTSRequest(r io.Reader) (tsRequest, error) {
	var req tsRequest
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return req, err
	}
	if header[0] != 0x30 {
		return req, errProtocol
	}
	p := append([]byte(nil), header[:]...)
	n := int(header[1])
	if n&128 != 0 {
		count := n & 127
		if count < 1 || count > 2 {
			return req, errProtocol
		}
		length := make([]byte, count)
		if _, err := io.ReadFull(r, length); err != nil {
			return req, err
		}
		p = append(p, length...)
		n = 0
		for _, b := range length {
			n = n<<8 | int(b)
		}
	}
	if n == 0 || n > maxCredSSP-len(p) {
		return req, errProtocol
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return req, err
	}
	rest, err := asn1.Unmarshal(append(p, body...), &req)
	if err != nil || len(rest) != 0 || len(req.NegoTokens) > 1 {
		return req, errProtocol
	}
	return req, nil
}

func (c *client) sendTSRequest(req tsRequest) error {
	req.Version = credSSPVersion
	p, err := asn1.Marshal(req)
	if err != nil {
		return err
	}
	return c.write(p)
}

func sameCredentials(user, pass, wantUser, wantPass string) bool {
	a, b := sha256.Sum256([]byte(user)), sha256.Sum256([]byte(wantUser))
	x, y := sha256.Sum256([]byte(pass)), sha256.Sum256([]byte(wantPass))
	return subtle.ConstantTimeCompare(a[:], b[:])&subtle.ConstantTimeCompare(x[:], y[:]) == 1
}

func credSSPBinding(publicKey, nonce []byte, direction string) []byte {
	h := sha256.New()
	h.Write([]byte("CredSSP " + direction + " Binding Hash\x00"))
	h.Write(nonce)
	h.Write(publicKey)
	return h.Sum(nil)
}

// Extract SubjectPublicKey, not the enclosing SubjectPublicKeyInfo, as required
// by MS-CSSP 3.1.5. certificate is the actual certificate selected by TLS.
func credSSPPublicKey(certificate *tls.Certificate) ([]byte, error) {
	if certificate == nil || len(certificate.Certificate) == 0 {
		return nil, errors.New("NLA requires the negotiated TLS certificate")
	}
	cert, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, err
	}
	var info struct {
		Algorithm asn1.RawValue
		Key       asn1.BitString
	}
	if _, err := asn1.Unmarshal(cert.RawSubjectPublicKeyInfo, &info); err != nil {
		return nil, err
	}
	return info.Key.RightAlign(), nil
}

func (c *client) authenticateNLA() (err error) {
	defer func() {
		if err != nil {
			// A CredSSP status lets clients report/retry failed credentials.
			_ = c.sendTSRequest(tsRequest{ErrorCode: 0xc000006d}) // STATUS_LOGON_FAILURE
		}
	}()
	publicKey, err := credSSPPublicKey(c.tlsCertificate)
	if err != nil {
		return err
	}
	req, err := readTSRequest(c.conn)
	if err != nil {
		return err
	}
	// Require the nonce binding added in CredSSP v5 (CVE-2018-0886).
	if req.Version < 5 || len(req.NegoTokens) != 1 || len(req.AuthInfo) != 0 || len(req.PubKeyAuth) != 0 || req.ErrorCode != 0 {
		return errors.New("NLA requires CredSSP version 5 or newer and NTLMv2")
	}
	negotiate, wrapped, err := unwrapNTLM(req.NegoTokens[0].Token)
	if err != nil {
		return err
	}
	a, err := newNTLMAcceptor(negotiate)
	if err != nil {
		return err
	}
	challenge := a.challenge
	if wrapped {
		challenge = wrapNTLM(challenge, false)
	}
	if err := c.sendTSRequest(tsRequest{NegoTokens: []negoToken{{Token: challenge}}}); err != nil {
		return err
	}
	version := req.Version
	req, err = readTSRequest(c.conn)
	if err != nil {
		return err
	}
	if req.Version != version || len(req.NegoTokens) != 1 || len(req.ClientNonce) != 32 || len(req.PubKeyAuth) < 16 || len(req.AuthInfo) != 0 || req.ErrorCode != 0 {
		return errProtocol
	}
	authenticate, authWrapped, mechMIC, err := unwrapNTLMWithMIC(req.NegoTokens[0].Token)
	if err != nil || authWrapped != wrapped {
		return errProtocol
	}
	runtime := c.s.runtimeConfig()
	security, err := a.authenticate(authenticate, runtime.Username, runtime.Password)
	if err != nil {
		return err
	}
	var responseMIC []byte
	if wrapped && len(mechMIC) != 0 {
		mechanisms, _ := asn1.Marshal([]asn1.ObjectIdentifier{ntlmOID})
		if err := security.verifyMIC(mechanisms, mechMIC); err != nil {
			return err
		}
		responseMIC = security.mechMIC(mechanisms)
	}
	binding, err := security.unseal(req.PubKeyAuth)
	if err != nil || !bytes.Equal(binding, credSSPBinding(publicKey, req.ClientNonce, "Client-To-Server")) {
		return errors.New("NLA TLS public-key binding failed")
	}
	response := tsRequest{PubKeyAuth: security.seal(credSSPBinding(publicKey, req.ClientNonce, "Server-To-Client"))}
	if wrapped {
		response.NegoTokens = []negoToken{{Token: wrapNTLM(nil, true, responseMIC)}}
	}
	if err := c.sendTSRequest(response); err != nil {
		return err
	}
	req, err = readTSRequest(c.conn)
	if err != nil {
		return err
	}
	if req.Version != version || len(req.AuthInfo) < 16 || len(req.NegoTokens) != 0 || len(req.PubKeyAuth) != 0 || req.ErrorCode != 0 {
		return errProtocol
	}
	p, err := security.unseal(req.AuthInfo)
	if err != nil {
		return err
	}
	var creds tsCredentials
	rest, err := asn1.Unmarshal(p, &creds)
	if err != nil || len(rest) != 0 || creds.Type != 1 {
		return errProtocol
	}
	var password tsPasswordCredentials
	rest, err = asn1.Unmarshal(creds.Credentials, &password)
	if err != nil || len(rest) != 0 {
		return errProtocol
	}
	domain, err := utf16String(password.Domain)
	if err != nil {
		return err
	}
	user, err := utf16String(password.Username)
	if err != nil {
		return err
	}
	pass, err := utf16String(password.Password)
	if err != nil {
		return err
	}
	if !strings.EqualFold(user, runtime.Username) || !sameCredentials(runtime.Username, pass, runtime.Username, runtime.Password) {
		return errNLAAuth
	}
	c.logonUser, c.logonDomain = user, domain
	return nil
}

var ntlmOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 2, 2, 10}
var spnegoOID = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 2}

// CredSSP normally carries raw NTLM messages; also accept explicitly selected
// NTLM in SPNEGO. Kerberos is unsupported.
func unwrapNTLM(p []byte) ([]byte, bool, error) {
	token, wrapped, _, err := unwrapNTLMWithMIC(p)
	return token, wrapped, err
}

func unwrapNTLMWithMIC(p []byte) ([]byte, bool, []byte, error) {
	if bytes.HasPrefix(p, ntlmSignature) {
		return p, false, nil, nil
	}
	var outer asn1.RawValue
	rest, err := asn1.Unmarshal(p, &outer)
	if err != nil || len(rest) != 0 {
		return nil, false, nil, errProtocol
	}
	if outer.Class == 1 && outer.Tag == 0 {
		var oid asn1.ObjectIdentifier
		rest, err = asn1.Unmarshal(outer.Bytes, &oid)
		if err != nil || !oid.Equal(spnegoOID) {
			return nil, false, nil, errProtocol
		}
		rest, err = asn1.Unmarshal(rest, &outer)
		if err != nil || len(rest) != 0 {
			return nil, false, nil, errProtocol
		}
	}
	if outer.Class != 2 || (outer.Tag != 0 && outer.Tag != 1) {
		return nil, false, nil, errProtocol
	}
	initial := outer.Tag == 0
	var sequence asn1.RawValue
	rest, err = asn1.Unmarshal(outer.Bytes, &sequence)
	if err != nil || len(rest) != 0 || sequence.Tag != 16 || sequence.Class != 0 {
		return nil, false, nil, errProtocol
	}
	var token []byte
	var mic []byte
	lastTag := -1
	for fields := sequence.Bytes; len(fields) > 0; {
		var field asn1.RawValue
		fields, err = asn1.Unmarshal(fields, &field)
		if err != nil || field.Class != 2 || !field.IsCompound || field.Tag <= lastTag {
			return nil, false, nil, errProtocol
		}
		lastTag = field.Tag
		switch field.Tag {
		case 0:
			if initial {
				var mechanisms []asn1.ObjectIdentifier
				rest, err = asn1.Unmarshal(field.Bytes, &mechanisms)
				if err != nil || len(rest) != 0 || len(mechanisms) != 1 || !mechanisms[0].Equal(ntlmOID) {
					return nil, false, nil, errors.New("NLA supports the NTLM SPNEGO mechanism only")
				}
			}
		case 1:
			if !initial {
				var oid asn1.ObjectIdentifier
				rest, err = asn1.Unmarshal(field.Bytes, &oid)
				if err != nil || len(rest) != 0 || !oid.Equal(ntlmOID) {
					return nil, false, nil, errProtocol
				}
			}
		case 2:
			rest, err = asn1.Unmarshal(field.Bytes, &token)
			if err != nil || len(rest) != 0 {
				return nil, false, nil, errProtocol
			}
		case 3:
			if initial {
				return nil, false, nil, errProtocol
			}
			rest, err = asn1.Unmarshal(field.Bytes, &mic)
			if err != nil || len(rest) != 0 || len(mic) != 16 {
				return nil, false, nil, errProtocol
			}
		default:
			return nil, false, nil, fmt.Errorf("unsupported NLA SPNEGO field %d", field.Tag)
		}
	}
	if !bytes.HasPrefix(token, ntlmSignature) {
		return nil, false, nil, errProtocol
	}
	return token, true, mic, nil
}

func wrapNTLM(token []byte, complete bool, mic ...[]byte) []byte {
	state := 1
	if complete {
		state = 0
	}
	enumerated, _ := asn1.Marshal(asn1.Enumerated(state))
	fields := berValue([]byte{0xa0}, enumerated)
	if !complete {
		oid, _ := asn1.Marshal(ntlmOID)
		encoded, _ := asn1.Marshal(token)
		fields = join(fields, berValue([]byte{0xa1}, oid), berValue([]byte{0xa2}, encoded))
	}
	if len(mic) != 0 && len(mic[0]) != 0 {
		encoded, _ := asn1.Marshal(mic[0])
		fields = join(fields, berValue([]byte{0xa3}, encoded))
	}
	return berValue([]byte{0xa1}, berValue([]byte{0x30}, fields))
}
