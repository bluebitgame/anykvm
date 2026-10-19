//go:build !cgo || (!amd64 && !arm64)

package playfair

import "errors"

func Available() error                       { return errors.New("airplay: FairPlay requires CGO on amd64 or arm64") }
func Decrypt([]byte, []byte) ([]byte, error) { return nil, Available() }
