package app

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestServerFlagsRequireYAMLSettings(t *testing.T) {
	for _, name := range []string{
		"vnc", "rdp", "rdp-username", "rdp-password", "http", "https",
		"gamestream", "gamestream-state", "tls-cert", "tls-key", "vnc-tls",
		"password", "pixel-tolerance", "force-continuous", "disable-awdl",
		"video", "video-size", "jpeg-quality", "audio", "hid", "mouse-mode",
	} {
		t.Run(name, func(t *testing.T) {
			fs := flag.NewFlagSet("server", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			registerServerFlags(fs)
			if fs.Lookup(name) != nil {
				t.Fatalf("service/device flag -%s is still registered", name)
			}
			if err := fs.Parse([]string{"-" + name + "=value"}); err == nil {
				t.Fatalf("removed service/device flag -%s was accepted", name)
			}
		})
	}
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	opts := registerServerFlags(fs)
	if err := fs.Parse([]string{"-config", "custom.yaml", "-gamestream-pair"}); err != nil {
		t.Fatal(err)
	}
	if opts.configPath != "custom.yaml" || !opts.gamestreamPair {
		t.Fatal("configuration selection and interactive pairing must remain available")
	}
}

func TestFirstRunConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, created, err := loadServerConfig(path, true)
	if err != nil || !created {
		t.Fatalf("first run must initialize missing default config: created=%t, err=%v", created, err)
	}
	if cfg.Web.HTTP != "127.0.0.1:8080" || cfg.Web.Password != "" ||
		cfg.VNC.Addr != "" || cfg.Web.HTTPS != "" || cfg.RDP.Addr != "" || cfg.GameStream.Addr != "" {
		t.Fatal("first run must enable only password-free Web on 127.0.0.1:8080")
	}
	if len(cfg.Targets) != 1 || cfg.Targets[0].HID.Device != "auto" || cfg.Targets[0].Audio.Source != "auto" {
		t.Fatal("first-run reference must include the default machine")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkCurrentServerConfig(data); err != nil {
		t.Fatalf("generated config violates the current schema/comment policy: %v", err)
	}
	got, created, err := loadServerConfig(path, true)
	if err != nil || created || !reflect.DeepEqual(got, cfg) {
		t.Fatalf("second startup must load the saved configuration: created=%t, err=%v", created, err)
	}
	generated := filepath.Join(t.TempDir(), "reference.yaml")
	if err := SaveDefaultConfig(generated); err != nil {
		t.Fatal(err)
	}
	exported, err := os.ReadFile(generated)
	if err != nil || string(exported) != string(data) {
		t.Fatalf("-gen-config and first startup must use the same reference: %v", err)
	}
}

func TestStartupConfigPreservesExistingFile(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		invalid    bool
	}{
		{"disabled_listeners", "vnc:\n  addr: \"\"\npassword: existing-password\n", false},
		{"invalid_yaml", "vnc: [\n", true},
		{"invalid_settings", "targets:\n  - name: Android\n    scrcpy:\n      crop: invalid\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, created, err := loadServerConfig(path, true)
			if created || (err != nil) != tc.invalid {
				t.Fatalf("existing config must be loaded or rejected: created=%t, err=%v", created, err)
			}
			if !tc.invalid && (cfg.VNC.Addr != "" || cfg.Web.Password != "") {
				t.Fatal("existing settings must not acquire a default VNC listener")
			}
			data, readErr := os.ReadFile(path)
			info, statErr := os.Stat(path)
			if readErr != nil || string(data) != tc.data || statErr != nil || info.Mode().Perm() != 0600 {
				t.Fatal("startup modified an existing config or its permissions")
			}
		})
	}
}

func TestStartupExplicitConfigMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom.yaml")
	_, created, err := loadServerConfig(path, false)
	if created || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing explicit config must fail: created=%t, err=%v", created, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("startup created an explicitly requested missing file")
	}
}
