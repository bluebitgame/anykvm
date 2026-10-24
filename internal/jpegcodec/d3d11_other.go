//go:build !windows || !cgo

package jpegcodec

// PrepareD3D11Shaders is only needed by the native Windows encoder.
func PrepareD3D11Shaders() error { return nil }
