// Package videodec shares native H.264 decoding across network sources.
package videodec

import "anykvm/internal/avmedia"

type Decoder interface {
	Decode([]byte) (*avmedia.Frame, error)
	Close()
}

func NewH264() (Decoder, error) {
	if d, err := newSystemVideoDecoder(); err == nil {
		return d, nil
	}
	return avmedia.NewDecoder("h264")
}

// SplitNALs accepts three-byte and four-byte Annex-B start codes.
func SplitNALs(data []byte) [][]byte {
	var result [][]byte
	start := -1
	for i := 0; i+2 < len(data); i++ {
		if data[i] != 0 || data[i+1] != 0 {
			continue
		}
		prefix := 0
		if data[i+2] == 1 {
			prefix = 3
		} else if i+3 < len(data) && data[i+2] == 0 && data[i+3] == 1 {
			prefix = 4
		}
		if prefix == 0 {
			continue
		}
		if start >= 0 {
			result = append(result, data[start:i])
		}
		start = i + prefix
		i = start - 1
	}
	if start >= 0 && start < len(data) {
		result = append(result, data[start:])
	}
	return result
}
