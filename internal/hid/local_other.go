//go:build !darwin && (!windows || !cgo)

package hid

import "fmt"

// openLocal returns an error on platforms without local input injection support.
// Local-machine-as-target currently supports macOS only.
func openLocal() (hidi, error) {
	return nil, fmt.Errorf("hid: local input injection not supported on this platform")
}
