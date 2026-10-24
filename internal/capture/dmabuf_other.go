//go:build !linux || !cgo

package capture

import "anykvm/internal/gpuconv"

func newDMABUFJPEG(uint32, uint32) (dmabufJPEGEncoder, error) { return nil, gpuconv.ErrUnavailable }

func supportsDMABUFVideoCodec(string) bool { return false }
