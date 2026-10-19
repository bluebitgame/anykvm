//go:build !cgo || (!darwin && !linux && !windows)

package rdp

func newNativeProgressive(string, int) (nativeProgressiveCodec, error) {
	return nil, errProgressiveNativeUnavailable
}
