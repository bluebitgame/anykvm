//go:build (!windows && !linux) || !cgo

package jpegcodec

func newComputeJPEGBackend() (computeJPEGBackend, error) { return nil, ErrComputeJPEG }
