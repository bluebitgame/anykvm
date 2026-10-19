//go:build linux

package service

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

func Manage(opts *Options) error {
	if os.Geteuid() == 0 {
		return errors.New("run -service as the desktop user, without sudo")
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" || !filepath.IsAbs(dir) {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dir = filepath.Join(home, ".config")
	}
	m := systemdServiceManager{filepath.Join(dir, "systemd", "user", systemdServiceName), runServiceCommand}
	switch opts.Action {
	case "install":
		cfg, err := prepareLoginService(opts)
		if err != nil {
			return err
		}
		if err := m.install(cfg); err != nil {
			return err
		}
		log.Printf("AnyKVM: desktop user service installed and started: %s; logs: journalctl --user -u %s", m.path, systemdServiceName)
	case "uninstall":
		if _, err := os.Stat(m.path); errors.Is(err, os.ErrNotExist) {
			log.Print("AnyKVM: user service is not installed")
			return nil
		} else if err != nil {
			return err
		}
		if _, err := m.command("disable", "--now", systemdServiceName); err != nil {
			return err
		}
		if err := os.Remove(m.path); err != nil {
			return err
		}
		_, err := m.command("daemon-reload")
		return err
	case "status":
		output, err := m.command("--no-pager", "status", systemdServiceName)
		fmt.Print(string(output))
		return err
	case "restart":
		_, err := m.command("restart", systemdServiceName)
		return err
	}
	return nil
}

func Run(*Options) (bool, error) { return false, nil }
