//go:build !darwin || !cgo

package airplay

import (
	"net"

	"github.com/grandcat/zeroconf"
)

func registerService(name, service string, port int, records []string, ifaces []net.Interface) (advertisement, error) {
	return zeroconf.Register(name, service, "local.", port, records, ifaces)
}
