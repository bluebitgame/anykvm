//go:build !cgo || (!linux && !darwin && !windows)

package videoenc

func newPlatformEncoder(Options) (Encoder, error) { return nil, ErrUnavailable }
