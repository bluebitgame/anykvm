package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"anykvm/internal/ard"
	"anykvm/internal/bridge"
)

func TestARDConfigAndSourceSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := `targets:
  - name: Mac
    video:
      source: ard://mac.example:5900
    ard:
      username: tester
      password_env: ANYKVM_MAC_PASSWORD
      codec: hevc
      display_mode: mirror
      timeout: 3s
      fallback: deny
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Targets[0]
	if m.ARD.Timeout != 3*time.Second || m.ARD.PasswordEnv != "ANYKVM_MAC_PASSWORD" {
		t.Fatal("ARD options not loaded")
	}
	dev, android := newCaptureSession(m)
	if _, ok := dev.(*ard.Client); !ok || android != nil {
		t.Fatal("ARD source not selected")
	}
	if _, ok := dev.(bridge.HIDSender); !ok {
		t.Fatal("ARD does not provide HID")
	}
	m.ARD.PasswordEnv = ""
	m.ARD.Password = "secret"
	cfg.Targets[0] = m
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("saved upstream password is world-readable")
	}
	m.Video.Source = "ard://user:secret@mac"
	cfg.Targets[0] = m
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	_, err = LoadConfig(path)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("URI credentials not safely rejected", err)
	}
	dev, _ = newCaptureSession(m)
	if err := dev.Open(); err == nil {
		t.Fatal("invalid ARD became another capture source")
	}
}
