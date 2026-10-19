package service

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// systemd parses quoting itself; shell quoting would not protect its expansions.
func systemdQuote(value string, command bool) string {
	value = strings.ReplaceAll(value, "%", "%%")
	if command {
		value = strings.ReplaceAll(value, "$", "$$")
	}
	return strconv.Quote(value)
}

func systemdServiceDefinition(cfg Config) []byte {
	var unit strings.Builder
	fmt.Fprintf(&unit, "[Unit]\nDescription=AnyKVM server\nPartOf=graphical-session.target\nAfter=graphical-session.target\nStartLimitIntervalSec=0\n\n[Service]\nType=simple\nExecStart=%s -config %s\nWorkingDirectory=%s\nRestart=always\nRestartSec=10\nTimeoutStopSec=30\n",
		systemdQuote(cfg.Executable, true), systemdQuote(cfg.Config, true), systemdQuote(cfg.WorkingDirectory, false))
	keys := make([]string, 0, len(cfg.Environment))
	for key := range cfg.Environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&unit, "Environment=%s\n", systemdQuote(key+"="+cfg.Environment[key], false))
	}
	unit.WriteString("\n[Install]\nWantedBy=graphical-session.target\n")
	return []byte(unit.String())
}

const systemdServiceName = "anykvm.service"

type systemdServiceManager struct {
	path string
	run  func(string, ...string) ([]byte, error)
}

func (m systemdServiceManager) command(args ...string) ([]byte, error) {
	return m.run("systemctl", append([]string{"--user"}, args...)...)
}

func (m systemdServiceManager) install(cfg Config) (err error) {
	// Check the user manager before changing a definition.
	if _, err := m.command("show-environment"); err != nil {
		return err
	}
	old, readErr := os.ReadFile(m.path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	_, activeErr := m.command("is-active", "--quiet", systemdServiceName)
	if err := writeServiceFile(m.path, systemdServiceDefinition(cfg), 0644); err != nil {
		return err
	}
	defer func() {
		if err == nil {
			return
		}
		if readErr == nil {
			err = errors.Join(err, writeServiceFile(m.path, old, 0644))
		} else {
			_, disableErr := m.command("disable", "--now", systemdServiceName)
			err = errors.Join(err, disableErr, os.Remove(m.path))
		}
		_, reloadErr := m.command("daemon-reload")
		err = errors.Join(err, reloadErr)
		if readErr == nil && activeErr == nil {
			_, restartErr := m.command("restart", systemdServiceName)
			err = errors.Join(err, restartErr)
		}
	}()
	// Do not pin DISPLAY or Wayland socket names into a unit: the desktop
	// imports fresh values at the next login. Import this session for --now.
	var desktopEnv []string
	for _, key := range []string{"DISPLAY", "WAYLAND_DISPLAY", "XAUTHORITY", "XDG_CURRENT_DESKTOP", "XDG_SESSION_TYPE", "XDG_SEAT"} {
		if _, ok := os.LookupEnv(key); ok {
			desktopEnv = append(desktopEnv, key)
		}
	}
	if len(desktopEnv) > 0 {
		if _, err = m.command(append([]string{"import-environment"}, desktopEnv...)...); err != nil {
			return err
		}
	}
	if _, err = m.command("daemon-reload"); err != nil {
		return err
	}
	if _, err = m.command("enable", systemdServiceName); err != nil {
		return err
	}
	_, err = m.command("restart", systemdServiceName)
	return err
}
