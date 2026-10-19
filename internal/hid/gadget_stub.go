//go:build !linux

package hid

import "fmt"

func openGadget() (hidi, error) {
	return nil, fmt.Errorf("gadget only supported on Linux")
}
