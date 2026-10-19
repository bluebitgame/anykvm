//go:build (!darwin && !windows && !linux) || !cgo

package capture

import "fmt"

func ListVideoModes(device string) ([]VideoMode, error) {
	return nil, fmt.Errorf("capture mode discovery is not available on this platform")
}
