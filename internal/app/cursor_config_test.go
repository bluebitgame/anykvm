package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCursorConfigDefaultsAndRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	for _, tc := range []struct{ line, rendering string }{
		{"", "client"},
		{"      cursor: remote\n", "remote"},
		{"      cursor: client\n", "client"},
	} {
		data := "targets:\n  - name: test\n    video:\n      source: screen:0\n" + tc.line
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Targets[0].Video.CursorRendering != tc.rendering {
			t.Fatal("incorrect cursor rendering default/value")
		}
		machines := buildMachines(cfg)
		if machines[0].CursorRendering != tc.rendering {
			t.Fatal("cursor rendering not passed to bridge")
		}
		for _, save := range []func(string, Config) error{SaveConfig, SaveWebConfig} {
			if err = save(path, cfg); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil || strings.Contains(string(raw), "hide_cursor") || strings.Contains(string(raw), "cursor_rendering") {
				t.Fatal("obsolete setting written", err)
			}
			cfg, err = LoadConfig(path)
			if err != nil || cfg.Targets[0].Video.CursorRendering != tc.rendering {
				t.Fatal("cursor rendering round trip", err)
			}
		}
	}
	if DefaultConfig().Targets[0].Video.CursorRendering != "client" {
		t.Fatal("default config cursor rendering")
	}
}

func TestCursorConfigRejectsRemovedAndInvalidSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	for _, line := range []string{"hide_cursor: true", "hide_cursor: false", "cursor_rendering: client", "cursor: server", "cursor: Client"} {
		if err := os.WriteFile(path, []byte("targets:\n  - video:\n      source: screen\n      "+line+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Fatalf("accepted %s", line)
		}
	}
	cfg := DefaultConfig()
	cfg.Targets[0].Video.CursorRendering = "invalid"
	if err := validateConfig(cfg); err == nil {
		t.Fatal("Web validation accepted invalid rendering")
	}
}
