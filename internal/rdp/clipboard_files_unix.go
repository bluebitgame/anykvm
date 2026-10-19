//go:build darwin || linux

package rdp

import (
	"os"

	"golang.org/x/sys/unix"
)

func openClipboardSource(root *os.Root, name string) (*os.File, error) {
	// A selected regular file replaced by a FIFO must not block the RDP reader.
	return root.OpenFile(name, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
}
