// Package avmedia provides shared media types and optional in-process FFmpeg
// support. Compile with the ffmpeg build tag and CGO to enable its backend.
package avmedia

import (
	"anykvm/internal/framebuf"
	"anykvm/internal/framerate"
	"errors"
)

var ErrUnavailable = errors.New("FFmpeg 9 shared libraries unavailable")

type Frame struct {
	Buffer        *framebuf.Buffer // non-nil only for ReadOwned; release after its final reader
	Data          []byte
	Width, Height int
	Codec         string // empty for decoded pixels/PCM; h264/hevc for Annex-B access units
}
type VideoOptions struct {
	Rate                        framerate.Rate // Exact rate; takes precedence over the integer FPS fallback.
	Width, Height, FPS, Bitrate int
	Codec, Device               string
}

func (f *Frame) Release() {
	if f != nil {
		f.Buffer.Release()
		f.Buffer = nil
		f.Data = nil
	}
}
