//go:build viewer

package viewer

import (
	"anykvm/internal/avmedia"
	"anykvm/internal/capture"
)

type decoder struct {
	session       *avmedia.Session
	width, height int
}

func newDecoder(_ string, w, h int, format capture.VideoFormat) (*decoder, error) {
	name := "h264"
	if format == capture.FormatHEVC {
		name = "hevc"
	}
	s, err := avmedia.NewDecoder(name)
	if err != nil {
		return nil, err
	}
	return &decoder{session: s, width: w, height: h}, nil
}
func (d *decoder) decode(data []byte) ([]byte, error) {
	f, err := d.session.Decode(data)
	if err != nil || f == nil {
		return nil, err
	}
	d.width = f.Width
	d.height = f.Height
	return f.Data, nil
}
func (d *decoder) close() { d.session.Close() }
