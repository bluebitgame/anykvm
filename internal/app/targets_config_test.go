package app

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestTargetsConfigCompatibility(t *testing.T) {
	legacy := "machines: [{name: Legacy, video: {source: 'screen:0', mode: '@30'}, hid: {device: local}}]\n"
	current := "targets: [{name: Current, video: {source: 'adb', mode: '@60'}}]\n"
	for _, tc := range []struct {
		name, data, targetName string
		count                  int
	}{
		{"omitted", "{}", "Default", 1},
		{"legacy", legacy, "Legacy", 1},
		{"current", current, "Current", 1},
		{"legacy_first", legacy + current, "Current", 1},
		{"current_first", current + legacy, "Current", 1},
		{"explicit_empty", legacy + "targets: []\n", "", 0},
		{"empty_first", "targets: []\n" + legacy, "", 0},
		{"explicit_null", legacy + "targets: null\n", "", 0},
		{"legacy_empty", "machines: []\n", "", 0},
		{"legacy_null", "machines: null\n", "", 0},
		{"ignored_obsolete_value", "machines: obsolete\n" + current, "Current", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.Targets) != tc.count || (tc.count > 0 && cfg.Targets[0].Name != tc.targetName) {
				t.Fatalf("unexpected targets after loading %s", tc.name)
			}
			if tc.name == "legacy" && (cfg.Targets[0].Video.Mode != "@30" || cfg.Targets[0].HID.Device != "local") {
				t.Fatal("legacy target settings were lost")
			}
			if err := SaveConfig(path, cfg); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]yaml.Node
			if err := yaml.Unmarshal(data, &document); err != nil {
				t.Fatal(err)
			}
			if _, ok := document["machines"]; ok {
				t.Fatal("saving wrote the legacy machines key")
			}
			if _, ok := document["targets"]; !ok {
				t.Fatal("saving omitted targets")
			}
			loaded, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if tc.count > 0 && !reflect.DeepEqual(loaded, cfg) {
				t.Fatal("target settings changed after saving and reloading")
			}
			if tc.count == 0 && len(loaded.Targets) != 0 {
				t.Fatal("saving an empty list restored the default target")
			}
		})
	}
}

func TestInvalidTargetsConfig(t *testing.T) {
	for _, data := range []string{
		"targets: not-a-list\n",
		"machines: not-a-list\n",
		"machines: [{name: Android, scrcpy: {crop: invalid}}]\n",
		"targets: [{name: Android, scrcpy: {crop: invalid}}]\n",
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Fatal("invalid targets must be rejected for both current and legacy keys")
		}
	}
}
