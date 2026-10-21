package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"anykvm/internal/spice"
)

func TestSPICEConfigSecretsAndSource(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Targets = []TargetConfig{{Name: "VM", Video: VideoConfig{Source: "spice:/run/user/1000/vm.sock"}, SPICE: spice.Options{Password: "private-ticket", RenderNode: "/dev/dri/renderD128"}}}
	if err := validateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	view := webConfigView(cfg)
	if view.Targets[0].SPICE.Password != retainedSecret {
		t.Fatal("SPICE password exposed to settings client")
	}
	view.restoreRetainedSecrets(cfg)
	if view.Targets[0].SPICE.Password != "private-ticket" {
		t.Fatal("SPICE credential was not retained")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("SPICE password saved with mode %o", info.Mode().Perm())
	}
	cfg.Targets[0].Video.Source = "spice://host:5900"
	if err = validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "VM") {
		t.Fatalf("missing named validation error: %v", err)
	}
}
