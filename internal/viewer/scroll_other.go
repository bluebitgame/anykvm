//go:build viewer && !darwin

package viewer

func platformNaturalWheelInvert() bool {
	return false
}
