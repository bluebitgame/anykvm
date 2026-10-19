//go:build !windows

package hid

// ProbeCH9329 identifies a controller by its serial GET_INFO response. It does
// not infer the protocol from a USB-serial bridge name such as CH340.
func ProbeCH9329(port string) bool {
	return probeSerialPort(port, baud9600, probeCH9329Connection)
}
