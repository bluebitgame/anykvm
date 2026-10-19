//go:build windows

package discovery

import "anykvm/internal/hid"

func platformSerialPorts() []string { return hid.ListSerialPorts() }
