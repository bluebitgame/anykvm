//go:build !darwin || !cgo

package videodec

import "anykvm/internal/avmedia"

func newSystemVideoDecoder() (Decoder, error) { return nil, avmedia.ErrUnavailable }
