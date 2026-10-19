//go:build !linux

package hid

import "fmt"

func openBluetooth() (hidi, error) {
	return nil, fmt.Errorf("bluetooth only supported on Linux")
}
