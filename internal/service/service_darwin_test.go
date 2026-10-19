//go:build darwin

package service

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"howett.net/plist"
)

func TestLaunchAgentDefinition(t *testing.T) {
	cfg := Config{Executable: "/apps/Any & KVM/server", Config: "/configs/test <1>.yaml", WorkingDirectory: "/work", Environment: map[string]string{"PATH": "/bin:/apps"}}
	data, err := launchAgentDefinition(cfg, "/logs")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Label, WorkingDirectory, LimitLoadToSessionType, StandardErrorPath string
		ProgramArguments                                                   []string
		RunAtLoad, KeepAlive                                               bool
	}
	if _, err := plist.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.ProgramArguments, []string{cfg.Executable, "-config", cfg.Config}) || got.Label != launchAgentLabel || !got.RunAtLoad || !got.KeepAlive || got.LimitLoadToSessionType != "Aqua" {
		t.Fatalf("invalid agent: %+v", got)
	}
}

func TestLaunchAgentInstallRollback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.plist")
	old := []byte("existing service definition")
	if err := os.WriteFile(path, old, 0644); err != nil {
		t.Fatal(err)
	}
	var calls []string
	fail := errors.New("bootstrap failed")
	bootstraps := 0
	m := launchAgentManager{path: path, logs: filepath.Join(dir, "logs"), domain: "gui/501", run: func(program string, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "bootstrap" {
			bootstraps++
			if bootstraps == 1 {
				return nil, fail
			}
			data, _ := os.ReadFile(path)
			if string(data) != string(old) {
				t.Fatal("old definition was not restored before recovery")
			}
		}
		return nil, nil
	}}
	err := m.install(Config{Executable: "/server", Config: "/config.yaml", WorkingDirectory: "/work"})
	if !errors.Is(err, fail) || bootstraps != 2 {
		t.Fatalf("rollback failed: err=%v, calls=%v", err, calls)
	}
	data, _ := os.ReadFile(path)
	if string(data) != string(old) {
		t.Fatal("old definition lost")
	}
}

func TestLaunchAgentInstallUpdatesLoadedService(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.plist")
	if err := os.WriteFile(path, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	var calls []string
	m := launchAgentManager{path: path, logs: filepath.Join(dir, "logs"), domain: "gui/501", run: func(_ string, args ...string) ([]byte, error) { calls = append(calls, args[0]); return nil, nil }}
	if err := m.install(Config{Executable: "/server", Config: "/config.yaml", WorkingDirectory: "/work"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"print", "bootout", "enable", "bootstrap"}) {
		t.Fatalf("wrong update sequence: %v", calls)
	}
}
