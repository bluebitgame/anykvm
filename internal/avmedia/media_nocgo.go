//go:build !ffmpeg || !cgo

package avmedia

import (
	"anykvm/internal/framebuf"
	"anykvm/internal/framerate"
)

type Session struct{}

func HasEncoder(string) bool { return false }

func NewEncoder(VideoOptions) (*Session, error)                  { return nil, Available() }
func NewAudioEncoder(int, int) (*Session, error)                 { return nil, Available() }
func NewDecoder(string) (*Session, error)                        { return nil, Available() }
func NewAudioDecoder(string, []byte, int, int) (*Session, error) { return nil, Available() }
func OpenSource(string, string, map[string]string, bool) (*Session, error) {
	return nil, Available()
}
func DeviceLog(string) []string                                        { return nil }
func (*Session) Encode([]byte, int, int, string, bool) ([]byte, error) { return nil, Available() }
func (*Session) Decode([]byte) (*Frame, error)                         { return nil, Available() }
func (*Session) Read() (*Frame, error)                                 { return nil, Available() }
func (*Session) Close()                                                {}

func OpenVideoSource(string, string, map[string]string, string) (*Session, error) {
	return nil, Available()
}

func (*Session) FrameRate() framerate.Rate { return framerate.Rate{} }

func (*Session) ReadOwned() (*Frame, error) { return nil, Available() }
func (*Session) EncodeOwned([]byte, int, int, string, bool) (*framebuf.Buffer, error) {
	return nil, Available()
}
