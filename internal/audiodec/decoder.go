// Package audiodec converts compressed source audio only for consumers needing PCM.
package audiodec

import "anykvm/internal/capture"

type Decoder interface {
	Decode([]byte) ([]byte, error)
	Close()
}

func New(f capture.AudioFrame) (Decoder, error) { return newDecoder(f) }
