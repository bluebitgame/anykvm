package playfair

import (
	"encoding/hex"
	"testing"
)

func TestSessionKeyMatchesUpstreamGolden(t *testing.T) {
	if err := Available(); err != nil {
		t.Skip(err)
	}
	m, e := make([]byte, 164), make([]byte, 72)
	for i := range m {
		m[i] = byte(i)
	}
	m[12] = 2
	for i := range e {
		e[i] = byte(255 - i)
	}
	key, err := Decrypt(m, e)
	// Obtained by compiling the unchanged upstream C files as a standalone
	// reference executable, rather than through AnyKVM's CGO wrapper.
	if err != nil || hex.EncodeToString(key) != "716949597631e422e8c7461c81666c2b" {
		t.Fatal(key, err)
	}
	for _, mode := range []byte{4, 255} {
		m[12] = mode
		if _, err := Decrypt(m, e); err == nil {
			t.Fatal("invalid mode accepted")
		}
	}
	if _, err := Decrypt(m[:163], e); err == nil {
		t.Fatal("truncated handshake accepted")
	}
}
