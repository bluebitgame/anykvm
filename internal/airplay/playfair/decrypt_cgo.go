//go:build cgo && (amd64 || arm64)

// Package playfair isolates the GPL v3 FairPlay session-key operation.
package playfair

/*
#cgo CFLAGS: -fno-strict-aliasing -Wno-unused-variable
#cgo linux LDFLAGS: -lm
#include "third_party/playfair.h"
*/
import "C"

import "fmt"

func Available() error { return nil }

func Decrypt(message, encrypted []byte) ([]byte, error) {
	if len(message) != 164 || len(encrypted) != 72 || message[12] > 3 {
		return nil, fmt.Errorf("airplay: invalid FairPlay key material")
	}
	m, e := append([]byte(nil), message...), append([]byte(nil), encrypted...)
	key := make([]byte, 16)
	// Synchronous C operation retains no Go pointers.
	C.playfair_decrypt((*C.uchar)(&m[0]), (*C.uchar)(&e[0]), (*C.uchar)(&key[0]))
	return key, nil
}
