//go:build !windows || !cgo

package capture

func ListWASAPILoopbackDevices() []string { return nil }
