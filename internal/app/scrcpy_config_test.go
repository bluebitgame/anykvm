package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"anykvm/internal/capture"
	"gopkg.in/yaml.v3"
)

func TestScrcpyGameStreamOrientationConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := "targets:\n  - name: Android\n    video:\n      source: adb\n    scrcpy:\n      gamestream_orientation: '@90'\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil || cfg.Targets[0].Scrcpy.GameStreamOrientation != "@90" {
		t.Fatalf("orientation load: %+v %v", cfg, err)
	}
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(path)
	if err != nil || cfg.Targets[0].Scrcpy.GameStreamOrientation != "@90" {
		t.Fatalf("orientation round trip: %+v %v", cfg, err)
	}
	data = strings.ReplaceAll(data, "'@90'", "'45'")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "scrcpy.gamestream_orientation") {
		t.Fatalf("invalid orientation not rejected with config field: %v", err)
	}
}

func TestScrcpyKeepActiveAudioAndHEVC(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		cfg := DefaultConfig()
		wire := "targets: [{name: Android, video: {source: adb}, scrcpy: {keep_active: true}}]"
		if !enabled {
			wire = "targets: [{name: Android, video: {source: adb}, audio: {source: ''}, scrcpy: {keep_active: false}}]"
		}
		if err := yaml.Unmarshal([]byte(wire), &cfg); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := SaveConfig(path, cfg); err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Targets[0].Scrcpy.KeepActive != enabled {
			t.Fatal("keep_active changed on save/load")
		}
		machines := buildMachines(loaded)
		if len(machines) != 1 || (machines[0].AudioDev != nil) != enabled {
			t.Fatal("Android audio auto/disabled selection failed")
		}
		dev := machines[0].Capture
		if _, ok := dev.(capture.VideoStreamConfigurer); !ok {
			t.Fatal("Android capture cannot receive GameStream codec settings")
		}
		if capability, ok := dev.(interface{ SupportsGameStreamHEVC() bool }); !ok || !capability.SupportsGameStreamHEVC() {
			t.Fatal("Android capture does not advertise HEVC")
		}
	}
}

func TestScrcpyIndependentDesktopConfig(t *testing.T) {
	for _, tc := range []struct {
		mode string
		w, h int
	}{
		{"", 0, 0}, {"@30", 0, 0}, {"auto@30", 0, 0},
		{"1920x1080@30", 1920, 1080}, {"720x1280", 720, 1280},
	} {
		cfg := DefaultConfig()
		cfg.Targets[0].Video.Source = "adb"
		cfg.Targets[0].Video.Mode = tc.mode
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := SaveConfig(path, cfg); err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadConfig(path)
		if err != nil || loaded.Targets[0].Video.Mode != tc.mode {
			t.Fatalf("mode round trip %q: %v", tc.mode, err)
		}
		machines := buildMachines(loaded)
		if len(machines) != 1 || machines[0].CapW != tc.w || machines[0].CapH != tc.h {
			t.Fatalf("mode %q did not preserve configured dimensions", tc.mode)
		}
		if _, ok := machines[0].Capture.(interface{ SetCaptureMode(string) error }); !ok {
			t.Fatalf("mode %q failed to configure Android capture", tc.mode)
		}
		machines[0].Capture.Close()
	}
}

func TestScrcpyCropConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := "targets:\n  - name: Android\n    video:\n      source: adb\n    scrcpy:\n      crop: '1272:2261:0:511'\n      gamestream_orientation: '270'\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil || cfg.Targets[0].Scrcpy.Crop != "1272:2261:0:511" {
		t.Fatalf("crop load: %+v %v", cfg, err)
	}
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(path)
	if err != nil || cfg.Targets[0].Scrcpy.Crop != "1272:2261:0:511" {
		t.Fatalf("crop round trip: %+v %v", cfg, err)
	}
	data = strings.ReplaceAll(data, "'1272:2261:0:511'", "'1272:2261:-1:511'")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "scrcpy.crop") || !strings.Contains(err.Error(), "Android") {
		t.Fatalf("invalid crop must identify its machine and config field: %v", err)
	}
	cfg.Targets[0].Scrcpy.Crop = ""
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("omitted crop must capture the full display: %v", err)
	}
}
