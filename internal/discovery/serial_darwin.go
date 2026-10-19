//go:build darwin

package discovery

import (
	"io/fs"
	"os"
	"strings"
)

func platformSerialPorts() []string {
	entries, _ := fs.ReadDir(os.DirFS("/dev"), ".")
	return macSerialPorts(entries)
}

func macSerialPorts(entries []fs.DirEntry) []string {
	var ports []string
	// Use call-out ports only. The corresponding tty.* nodes can wait for
	// carrier detect, and probing both names opens the same device twice.
	for _, e := range entries {
		n := e.Name()
		if !strings.HasPrefix(n, "cu.") {
			continue
		}
		lower := strings.ToLower(n)
		if strings.Contains(lower, "bluetooth") || lower == "cu.blth" {
			continue // macOS Bluetooth virtual ports are not serial HID controllers.
		}
		ports = append(ports, "/dev/"+n)
	}
	return ports
}
