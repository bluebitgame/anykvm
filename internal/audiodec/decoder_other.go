//go:build !darwin || !cgo

package audiodec

import (
	"anykvm/internal/avmedia"
	"anykvm/internal/capture"
	"fmt"
)

type avDecoder struct{ s *avmedia.Session }

func newDecoder(f capture.AudioFrame) (Decoder, error) {
	codec, config := "aac", f.Config
	if f.Codec == "alac" {
		cookie, err := alacMagicCookie(f)
		if err != nil {
			return nil, err
		}
		codec = "alac"
		config = append([]byte{0, 0, 0, 36, 'a', 'l', 'a', 'c', 0, 0, 0, 0}, cookie...)
	} else if f.Codec != "mp4a.40.39" && f.Codec != "mp4a.40.2" {
		return nil, fmt.Errorf("audio: unsupported codec %q", f.Codec)
	}
	s, e := avmedia.NewAudioDecoder(codec, config, f.SampleRate, f.Channels)
	if e != nil {
		return nil, e
	}
	return &avDecoder{s}, nil
}
func (d *avDecoder) Decode(p []byte) ([]byte, error) {
	f, e := d.s.Decode(p)
	if f == nil {
		return nil, e
	}
	return f.Data, e
}
func (d *avDecoder) Close() { d.s.Close() }
