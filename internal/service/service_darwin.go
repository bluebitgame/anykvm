//go:build darwin

package service

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"howett.net/plist"
)

const launchAgentLabel = "com.anykvm.server"

func launchAgentDefinition(cfg Config, logs string) ([]byte, error) {
	return plist.Marshal(map[string]any{
		"Label":                  launchAgentLabel,
		"ProgramArguments":       []string{cfg.Executable, "-config", cfg.Config},
		"WorkingDirectory":       cfg.WorkingDirectory,
		"EnvironmentVariables":   cfg.Environment,
		"LimitLoadToSessionType": "Aqua",
		"RunAtLoad":              true, "KeepAlive": true, "ThrottleInterval": 10,
		"StandardOutPath":   filepath.Join(logs, "stdout.log"),
		"StandardErrorPath": filepath.Join(logs, "stderr.log"),
	}, plist.XMLFormat)
}

type launchAgentManager struct {
	path, logs, domain string
	run                func(string, ...string) ([]byte, error)
}

func (m launchAgentManager) command(args ...string) ([]byte, error) {
	return m.run("/bin/launchctl", args...)
}

func (m launchAgentManager) loaded() (bool, error) {
	_, err := m.command("print", m.domain+"/"+launchAgentLabel)
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 113 {
		return false, nil
	}
	return false, err
}

func (m launchAgentManager) install(cfg Config) (err error) {
	data, err := launchAgentDefinition(cfg, m.logs)
	if err != nil {
		return err
	}
	loaded, err := m.loaded()
	if err != nil {
		return err
	}
	old, readErr := os.ReadFile(m.path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if loaded && readErr != nil {
		return fmt.Errorf("loaded service definition is missing: %s", m.path)
	}
	if err := os.MkdirAll(m.logs, 0755); err != nil {
		return err
	}
	if err := writeServiceFile(m.path, data, 0644); err != nil {
		return err
	}
	stopped := false
	defer func() {
		if err == nil {
			return
		}
		if readErr == nil {
			err = errors.Join(err, writeServiceFile(m.path, old, 0644))
		} else {
			err = errors.Join(err, os.Remove(m.path))
		}
		if stopped {
			_, restoreErr := m.command("bootstrap", m.domain, m.path)
			err = errors.Join(err, restoreErr)
		}
	}()
	if loaded {
		if _, err = m.command("bootout", m.domain+"/"+launchAgentLabel); err != nil {
			return err
		}
		stopped = true
	}
	if _, err = m.command("enable", m.domain+"/"+launchAgentLabel); err != nil {
		return err
	}
	_, err = m.command("bootstrap", m.domain, m.path)
	return err
}

func Manage(opts *Options) error {
	if os.Geteuid() == 0 {
		return errors.New("run -service as the logged-in user, without sudo")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	m := launchAgentManager{
		path:   filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist"),
		logs:   filepath.Join(home, "Library", "Logs", "AnyKVM"),
		domain: "gui/" + strconv.Itoa(os.Getuid()), run: runServiceCommand,
	}
	switch opts.Action {
	case "install":
		cfg, err := prepareLoginService(opts)
		if err != nil {
			return err
		}
		if err := m.install(cfg); err != nil {
			return err
		}
		log.Printf("AnyKVM: login service installed and started: %s; logs: %s", m.path, m.logs)
	case "uninstall":
		loaded, err := m.loaded()
		if err != nil {
			return err
		}
		if loaded {
			if _, err := m.command("bootout", m.domain+"/"+launchAgentLabel); err != nil {
				return err
			}
		}
		if err := os.Remove(m.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		log.Print("AnyKVM: login service uninstalled; configuration and logs retained")
	case "status":
		output, err := m.command("print", m.domain+"/"+launchAgentLabel)
		fmt.Print(string(output))
		return err
	case "restart":
		_, err := m.command("kickstart", "-k", m.domain+"/"+launchAgentLabel)
		return err
	}
	return nil
}

func Run(*Options) (bool, error) { return false, nil }
