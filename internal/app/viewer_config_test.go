package app

import (
	"gopkg.in/yaml.v3"
	"path/filepath"
	"testing"
)

func TestViewerConfigRoundTrip(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		cfg := DefaultConfig()
		cfg.Viewer = enabled
		cfg.Targets = []TargetConfig{{Name: "One"}, {Name: "Two"}}
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := SaveWebConfig(path, cfg); err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Viewer != enabled || len(loaded.Targets) != 2 {
			t.Fatalf("loaded config = %+v", loaded)
		}
	}
	cfg := DefaultConfig()
	if err := yaml.Unmarshal([]byte("targets: []\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Viewer {
		t.Fatal("viewer should default to false")
	}
}
