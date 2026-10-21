//go:build (!linux && !windows) || !cgo

package gpuconv

type ProgressiveCompute struct{}

func NewProgressiveCompute(string, [4][]byte) (*ProgressiveCompute, error) {
	return nil, ErrUnavailable
}
func (*ProgressiveCompute) Run([]byte, []uint32) ([]byte, [48]uint32, error) {
	return nil, [48]uint32{}, ErrUnavailable
}
func (*ProgressiveCompute) Close()                                {}
func (*ProgressiveCompute) ConfigureSurface(string, []byte) error { return ErrUnavailable }
func (*ProgressiveCompute) PrepareSurface(*Surface, int, int, bool) ([]uint32, bool, error) {
	return nil, false, ErrUnavailable
}
func (*ProgressiveCompute) RunSurface([]uint32) ([]byte, [48]uint32, error) {
	return nil, [48]uint32{}, ErrUnavailable
}
func (*ProgressiveCompute) CommitSurface() error { return ErrUnavailable }
