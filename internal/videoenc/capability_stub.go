//go:build !cgo || (!linux && !darwin && !windows)

package videoenc

func platformSupportsCodec(string) bool { return false }
