package service

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestServiceWorkerEnvironment(t *testing.T) {
	got := serviceWorkerEnvironment([]string{"Path=old", "KEEP=value", "=C:=C:\\old", WorkerStopEnvironment + "=stale"}, map[string]string{"PATH": "new"}, "new-event", "Winlogon")
	want := []string{"=C:=C:\\old", workerDesktopEnvironment + "=Winlogon", WorkerStopEnvironment + "=new-event", "KEEP=value", "PATH=new"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q; want %q", got, want)
	}
	if serviceDesktopExit("default") != workerExitDefault || serviceDesktopExit("WINLOGON") != workerExitWinlogon || serviceDesktopExit("custom") != 0 {
		t.Fatal("unexpected desktop transition policy")
	}
}

func TestSystemdDefinitionEscapesPaths(t *testing.T) {
	cfg := Config{Executable: "/apps/Any KVM/server", Config: "/configs/$secret%n\".yaml", WorkingDirectory: "/work/100% done", Environment: map[string]string{"PATH": "/bin:/apps/100%"}}
	unit := string(systemdServiceDefinition(cfg))
	for _, expected := range []string{
		`ExecStart="/apps/Any KVM/server" -config "/configs/$$secret%%n\".yaml"`,
		`WorkingDirectory="/work/100%% done"`, `Environment="PATH=/bin:/apps/100%%"`,
		"PartOf=graphical-session.target", "WantedBy=graphical-session.target", "Restart=always",
	} {
		if !strings.Contains(unit, expected) {
			t.Fatalf("missing %q in unit:\n%s", expected, unit)
		}
	}
	if strings.Contains(unit, "Environment=DISPLAY") {
		t.Fatal("desktop socket names must not be pinned into the unit")
	}
}

func TestWriteServiceFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents", "service.plist")
	if err := writeServiceFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeServiceFile(path, []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "new" {
		t.Fatal("definition was not replaced")
	}
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".anykvm-service-*"))
	if len(matches) != 0 {
		t.Fatal("temporary files were left behind")
	}
}

func TestSystemdInstallRollback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "anykvm.service")
	old := []byte("existing definition")
	if err := os.WriteFile(path, old, 0644); err != nil {
		t.Fatal(err)
	}
	restarts := 0
	failure := errors.New("restart failed")
	m := systemdServiceManager{path: path, run: func(_ string, args ...string) ([]byte, error) {
		if args[0] != "--user" {
			t.Fatal("system service manager was used")
		}
		if args[1] == "restart" {
			restarts++
			if restarts == 1 {
				return nil, failure
			}
			data, _ := os.ReadFile(path)
			if string(data) != string(old) {
				t.Fatal("restart before restoring old unit")
			}
		}
		return nil, nil
	}}
	err := m.install(Config{Executable: "/server", Config: "/config.yaml", WorkingDirectory: "/work"})
	if !errors.Is(err, failure) || restarts != 2 {
		t.Fatalf("rollback failed: %v, restarts=%d", err, restarts)
	}
	data, _ := os.ReadFile(path)
	if string(data) != string(old) {
		t.Fatal("old unit lost")
	}
}

func TestSystemdMissingUserManagerDoesNotWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anykvm.service")
	failure := errors.New("user manager unavailable")
	m := systemdServiceManager{path: path, run: func(_ string, _ ...string) ([]byte, error) { return nil, failure }}
	if err := m.install(Config{}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unit was written despite missing systemd")
	}
}
