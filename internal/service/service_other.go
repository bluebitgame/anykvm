//go:build !darwin && !linux && !windows

package service

import "errors"

func Manage(*Options) error {
	return errors.New("automatic startup is supported on macOS, Linux with systemd, and Windows")
}

func Run(*Options) (bool, error) { return false, nil }
