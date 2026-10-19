//go:build !ffmpeg || !cgo

package videoenc

import "anykvm/internal/avmedia"

func openAV(Options, string, string) (Encoder, error) {
	return nil, avmedia.Available()
}
