package app

import (
	"anykvm/internal/service"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestServiceFlags(t *testing.T) {
	for _, action := range []string{"install", "uninstall", "status", "restart"} {
		fs := flag.NewFlagSet("server", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		opts := registerServerFlags(fs)
		if err := fs.Parse([]string{"-service", action}); err != nil {
			t.Fatal(err)
		}
		if opts.serviceAction != action || opts.validateServiceAction() != nil {
			t.Fatalf("action %s was rejected", action)
		}
	}
	for _, opts := range []serverOptions{
		{serviceAction: "unknown"}, {serviceAction: "install", listDevices: true},
		{serviceAction: "install", listIOS: true}, {serviceAction: "install", pairSpec: "ble:device:0"},
		{serviceAction: "install", genConfig: "reference.yaml"}, {serviceAction: "install", gamestreamPair: true},
		{serviceAction: "restart", configPath: "other.yaml"},
	} {
		if opts.validateServiceAction() == nil {
			t.Fatalf("conflicting options accepted: %+v", opts)
		}
	}
	fs := flag.NewFlagSet("console", flag.ContinueOnError)
	opts := registerServerFlags(fs)
	if err := fs.Parse([]string{"-config", "custom.yaml"}); err != nil {
		t.Fatal(err)
	}
	if opts.serviceAction != "" || opts.validateServiceAction() != nil {
		t.Fatal("ordinary console startup changed")
	}
}

func TestServiceConfigSelection(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "anykvm")
	if _, err := resolveLoginService("missing.yaml", executable, dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing explicit config: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "missing.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("explicit config was created")
	}
	got, err := resolveLoginService("", executable, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Config != filepath.Join(dir, "config.yaml") || got.WorkingDirectory != dir {
		t.Fatalf("wrong service paths: %+v", got)
	}
	data := []byte("vnc: {addr: ''}\npassword: preserve-this\n")
	path := filepath.Join(dir, "custom & config.yaml")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	got, err = resolveLoginService(filepath.Base(path), executable, dir)
	if err != nil || got.Config != path {
		t.Fatalf("relative config failed: %+v, %v", got, err)
	}
	after, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	if string(after) != string(data) || info.Mode().Perm() != 0600 {
		t.Fatal("registration modified configuration or permissions")
	}
	if _, ok := got.Environment[service.WorkerStopEnvironment]; ok {
		t.Fatal("worker controls must not be persisted at installation")
	}
}
