//go:build ffmpeg && cgo

package videoenc

import (
	"anykvm/internal/avmedia"
	"anykvm/internal/framebuf"
)

type avEncoder struct {
	session *avmedia.Session
	options Options
	name    string
}

func openAV(o Options, name, device string) (Encoder, error) {
	s, err := avmedia.NewEncoder(avmedia.VideoOptions{Width: o.Width, Height: o.Height, FPS: o.FPS, Rate: o.Rate, Bitrate: o.Bitrate, Codec: name, Device: device})
	if err != nil {
		return nil, err
	}
	return &avEncoder{session: s, options: o, name: name}, nil
}
func (e *avEncoder) Encode(data []byte, idr bool) ([]byte, error) {
	return e.session.Encode(data, e.options.Width, e.options.Height, "nv12", idr)
}
func (e *avEncoder) EncodePixels(data []byte, w, h int, pixfmt string, idr bool) ([]byte, error) {
	return e.session.Encode(data, w, h, pixfmt, idr)
}
func (e *avEncoder) Close()       { e.session.Close() }
func (e *avEncoder) Name() string { return e.name }

func (e *avEncoder) EncodePixelsOwned(data []byte, w, h int, pixfmt string, idr bool) (*framebuf.Buffer, error) {
	return e.session.EncodeOwned(data, w, h, pixfmt, idr)
}
