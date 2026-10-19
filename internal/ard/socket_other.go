//go:build !darwin && !linux

package ard

import "syscall"

// Cross-host symmetric-port connections do not require socket reuse. Same-host
// media may fail explicitly on platforms without this socket option.
func reuseMediaPort(network, address string, c syscall.RawConn) error { return nil }
