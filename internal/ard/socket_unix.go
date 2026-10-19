//go:build darwin || linux

package ard

import (
	"golang.org/x/sys/unix"
	"syscall"
)

// Apple's media uses symmetric ports. Reuse also permits a loopback server
// and viewer to bind the same port, with connected UDP filtering the peer.
func reuseMediaPort(network, address string, c syscall.RawConn) error {
	var err error
	if controlErr := c.Control(func(fd uintptr) {
		err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
		if err == nil {
			err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		}
	}); controlErr != nil {
		return controlErr
	}
	return err
}
