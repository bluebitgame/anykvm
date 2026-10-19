package airplay

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"fmt"
	"net"
)

// Each control connection owns its handshake state. Never accept the shared
// secret for media until the sender has proved possession of its signing key.
type pairing struct {
	private              ed25519.PrivateKey
	verified             bool
	peerSigning          ed25519.PublicKey
	ours, theirs, secret []byte
	stream               cipher.Stream
	fairplaySetup        bool
	fairplayMessage      []byte
	media                *mediaSession
	control              net.Conn
}

func (p *pairing) pairSetup(body []byte) ([]byte, error) {
	if len(body) != ed25519.PublicKeySize || p.media != nil {
		return nil, fmt.Errorf("airplay: legacy pair-setup requires 32 bytes")
	}
	*p = pairing{private: p.private, control: p.control}
	return append([]byte(nil), p.private.Public().(ed25519.PublicKey)...), nil
}

func (p *pairing) fpSetup(body []byte) ([]byte, error) {
	if !p.verified || len(body) < 12 || string(body[:4]) != "FPLY" || body[4] != 3 || body[5] != 1 || p.media != nil {
		return nil, fmt.Errorf("airplay: invalid FairPlay version/state")
	}
	switch {
	case len(body) == 16 && body[6] == 1 && body[14] < 4:
		p.fairplaySetup, p.fairplayMessage = true, nil
		return append([]byte(nil), fairplayReplies[body[14]][:]...), nil
	case len(body) == 164 && body[6] == 3 && body[12] < 4 && p.fairplaySetup:
		p.fairplayMessage = append([]byte(nil), body...)
		return append(append([]byte(nil), fairplayHeader...), body[144:]...), nil
	default:
		return nil, fmt.Errorf("airplay: invalid FairPlay handshake")
	}
}

func (p *pairing) pairVerify(body []byte) ([]byte, error) {
	// A reconnecting sender can already know our Ed25519 public key from
	// Bonjour or an earlier pair-setup. Setup does not authenticate the sender;
	// the second verify message still must prove its signing-key possession.
	if len(body) < 4 || body[1] != 0 || body[2] != 0 || body[3] != 0 || p.media != nil {
		return nil, fmt.Errorf("airplay: invalid pair-verify state/header")
	}
	switch body[0] {
	case 1:
		if len(body) != 68 || p.stream != nil || p.verified {
			return nil, fmt.Errorf("airplay: invalid pair-verify first message")
		}
		key, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		peer, err := ecdh.X25519().NewPublicKey(body[4:36])
		if err != nil {
			return nil, err
		}
		secret, err := key.ECDH(peer)
		if err != nil {
			return nil, err
		}
		p.ours, p.theirs = key.PublicKey().Bytes(), append([]byte(nil), body[4:36]...)
		p.peerSigning, p.secret = append(ed25519.PublicKey(nil), body[36:]...), secret
		derive := func(label string) []byte {
			h := sha512.New()
			h.Write([]byte(label))
			h.Write(secret)
			return h.Sum(nil)[:aes.BlockSize]
		}
		block, err := aes.NewCipher(derive("Pair-Verify-AES-Key"))
		if err != nil {
			return nil, err
		}
		p.stream = cipher.NewCTR(block, derive("Pair-Verify-AES-IV"))
		sig := ed25519.Sign(p.private, append(append([]byte(nil), p.ours...), p.theirs...))
		p.stream.XORKeyStream(sig, sig)
		return append(append([]byte(nil), p.ours...), sig...), nil
	case 0:
		if len(body) != 68 || p.stream == nil || p.verified {
			return nil, fmt.Errorf("airplay: invalid pair-verify final message")
		}
		sig := make([]byte, ed25519.SignatureSize)
		p.stream.XORKeyStream(sig, body[4:])
		p.stream = nil
		message := append(append([]byte(nil), p.theirs...), p.ours...)
		if !ed25519.Verify(p.peerSigning, message, sig) {
			p.secret = nil
			return nil, fmt.Errorf("airplay: sender pair-verify signature failed")
		}
		p.verified = true
		return nil, nil
	default:
		return nil, fmt.Errorf("airplay: unknown pair-verify message")
	}
}
