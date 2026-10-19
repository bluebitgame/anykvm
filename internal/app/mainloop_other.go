//go:build !darwin

package app

func runApplicationMain(cfg Config, fn func()) { fn() }
