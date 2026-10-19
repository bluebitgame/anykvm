package app

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

type serverOptions struct {
	configPath     string
	genConfig      string
	listDevices    bool
	listIOS        bool
	pairSpec       string
	pairMouseMode  string
	gamestreamPair bool
	serviceAction  string
	serviceStop    <-chan struct{}
	serviceDone    func()
}

// Service and device settings are read exclusively from YAML.
func registerServerFlags(fs *flag.FlagSet) *serverOptions {
	opts := &serverOptions{}
	fs.StringVar(&opts.configPath, "config", "", "YAML config file (default: config.yaml beside the executable; created if missing)")
	fs.StringVar(&opts.genConfig, "gen-config", "", "write the default configuration (Web on 127.0.0.1:8080, other listeners disabled) to this path and exit")
	fs.BoolVar(&opts.listDevices, "list-devices", false, "list capture/audio/HID devices and exit")
	fs.BoolVar(&opts.listIOS, "list-ios", false, "include iPhone/iPad USB mirrors with -list-devices")
	fs.StringVar(&opts.pairSpec, "pair", "", "pair BLE profile and exit (e.g. ble:cu.usbmodem14101:0)")
	fs.StringVar(&opts.pairMouseMode, "pair-mouse-mode", "absolute", "mouse mode for -pair only: absolute or relative")
	fs.BoolVar(&opts.gamestreamPair, "gamestream-pair", false, "allow GameStream pairing by entering its PIN in this terminal")
	fs.StringVar(&opts.serviceAction, "service", "", "automatic startup: install, uninstall, status, or restart (then exit)")
	return opts
}

func (opts *serverOptions) validateServiceAction() error {
	if opts.serviceAction == "" {
		return nil
	}
	switch opts.serviceAction {
	case "install", "uninstall", "status", "restart":
	default:
		return fmt.Errorf("unknown -service action %q; use install, uninstall, status, or restart", opts.serviceAction)
	}
	if opts.genConfig != "" || opts.listDevices || opts.listIOS || opts.pairSpec != "" || opts.gamestreamPair {
		return errors.New("-service cannot be combined with generation, discovery, or pairing actions")
	}
	if opts.configPath != "" && opts.serviceAction != "install" {
		return errors.New("-config is only supported with -service install")
	}
	return nil
}

func serverConfigPath(path, executable string) (string, error) {
	if path == "" {
		path = filepath.Join(filepath.Dir(executable), "config.yaml")
	}
	return filepath.Abs(path)
}

// Create a missing default config only. Invalid files and missing explicit
// paths return an error without writing to disk.
func loadServerConfig(path string, initialize bool) (Config, bool, error) {
	cfg, err := LoadConfig(path)
	if err == nil || !initialize || !errors.Is(err, os.ErrNotExist) {
		return cfg, false, err
	}
	data, err := defaultConfigYAML()
	if err != nil {
		return cfg, false, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if errors.Is(err, os.ErrExist) {
		cfg, err = LoadConfig(path)
		return cfg, false, err
	}
	if err != nil {
		return cfg, false, fmt.Errorf("initialize config %s: %w", path, err)
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return cfg, false, fmt.Errorf("write initial config %s: %w", path, errors.Join(err, os.Remove(path)))
	}
	cfg, err = LoadConfig(path)
	return cfg, true, err
}
