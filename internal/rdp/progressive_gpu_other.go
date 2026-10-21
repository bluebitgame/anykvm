//go:build !linux || !cgo

package rdp

func progressiveSPIRV() [4][]byte     { return [4][]byte{} }
func progressiveSurfaceSPIRV() []byte { return nil }
