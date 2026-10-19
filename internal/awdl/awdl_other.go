//go:build !darwin

package awdl

func newSystemInterface() networkInterface { return nil }
