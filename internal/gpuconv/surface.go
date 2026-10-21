package gpuconv

import "fmt"

// Surface describes an immutable GPU snapshot. Its owning framebuf reference
// keeps the texture/Image alive; copying this descriptor does not retain it.
// Windows snapshots use a keyed-mutex shared RGB texture. Linux snapshots use
// a Converter Image on its original device, without exporting pixels to Go.
type Surface struct {
	Width, Height int
	SharedTexture uintptr
	AdapterLUID   uint64
	Rotation      int
	Image         *Image
}

func (s *Surface) Validate() error {
	if s == nil || s.Width < 1 || s.Height < 1 || s.Width > 8192 || s.Height > 8192 ||
		s.Width*s.Height > (64<<20)/4 || (s.SharedTexture == 0) == (s.Image == nil) ||
		(s.Rotation != 0 && s.Rotation != 90 && s.Rotation != 180 && s.Rotation != 270) {
		return fmt.Errorf("invalid immutable GPU surface")
	}
	return nil
}

// SurfaceParameters is the shared surface shader's 12-word ABI. Integer fitting
// and bilinear resampling use the same 8-bit weights as RDP's CPU scaler.
func SurfaceParameters(s *Surface, w, h int, force bool) ([12]uint32, error) {
	var p [12]uint32
	if err := s.Validate(); err != nil {
		return p, err
	}
	if w < 1 || h < 1 || w > 8192 || h > 8192 || w*h > (64<<20)/4 {
		return p, fmt.Errorf("invalid GPU surface output size")
	}
	p[0], p[1] = uint32(s.Width), uint32(s.Height)
	if s.Rotation == 90 || s.Rotation == 270 {
		p[0], p[1] = p[1], p[0]
	}
	p[3], p[4], p[5] = uint32(s.Rotation), uint32(w), uint32(h)
	dw, dh := w, h
	if s.Width*h > s.Height*w {
		dh = max(1, s.Height*w/s.Width)
	} else {
		dw = max(1, s.Width*h/s.Height)
	}
	p[6], p[7], p[8], p[9] = uint32((w-dw)/2), uint32((h-dh)/2), uint32(dw), uint32(dh)
	if force {
		p[10] = 1
	}
	return p, nil
}
