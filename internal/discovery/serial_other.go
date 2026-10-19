//go:build !darwin && !windows

package discovery

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func platformSerialPorts() []string {
	// Linux: /dev/ttyUSB*, /dev/ttyACM*, /dev/ttyS*
	// Also check /dev/serial/by-id/ if available.
	entries, _ := fs.ReadDir(os.DirFS("/dev"), ".")
	var ports []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, "ttyUSB") || strings.HasPrefix(n, "ttyACM") || strings.HasPrefix(n, "ttyS") {
			ports = append(ports, filepath.Join("/dev", n))
		}
	}
	byID, _ := fs.ReadDir(os.DirFS("/dev/serial/by-id"), ".")
	for _, e := range byID {
		p, err := filepath.EvalSymlinks(filepath.Join("/dev/serial/by-id", e.Name()))
		if err == nil {
			ports = append(ports, p)
		}
	}
	return ports
}
