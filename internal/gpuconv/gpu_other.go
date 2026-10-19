//go:build !linux || !cgo

package gpuconv

import "anykvm/internal/dmabuf"

type Converter struct{}
type Image struct{}

func New(uint32, uint32) (*Converter, error)                            { return nil, ErrUnavailable }
func (*Converter) Convert(dmabuf.Frame, int, int, bool) (*Image, error) { return nil, ErrUnavailable }
func (*Converter) Close()                                               {}
func (*Image) Readback() ([]byte, error)                                { return nil, ErrUnavailable }
func (*Image) Close()                                                   {}
