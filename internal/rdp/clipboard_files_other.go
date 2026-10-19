//go:build !darwin && !linux

package rdp

import "os"

func openClipboardSource(root *os.Root, name string) (*os.File, error) {
	return root.Open(name)
}
