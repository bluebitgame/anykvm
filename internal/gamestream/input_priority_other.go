//go:build !darwin || !cgo

package gamestream

func prioritizeInputThread() func() { return func() {} }
