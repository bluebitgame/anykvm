package app

import (
	"gopkg.in/yaml.v3"
	"path/filepath"
	"testing"
)

func TestVideoBitrateConfiguration(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Web.Bitrate != 12000 || cfg.VNC.Bitrate != 12000 {
		t.Fatal("existing video bitrate default changed")
	}
	if err := yaml.Unmarshal([]byte("web: {http: ':8080', bitrate: 24000}\nvnc: {addr: ':5900', bitrate: 18000}\nrdp: {addr: ':3389', bitrate: 20000}\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := SaveWebConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Web.Bitrate != 24000 || loaded.VNC.Bitrate != 18000 || loaded.RDP.Bitrate != 20000 {
		t.Fatal("protocol bitrate values did not survive save/load")
	}
	for _, value := range []int{-1, 1000001} {
		invalid := DefaultConfig()
		invalid.Web.Bitrate = value
		if validateConfig(invalid) == nil {
			t.Fatal("invalid Web bitrate accepted")
		}
		invalid = DefaultConfig()
		invalid.VNC.Bitrate = value
		if validateConfig(invalid) == nil {
			t.Fatal("invalid VNC bitrate accepted")
		}
	}
	previous := DefaultConfig()
	next := previous
	next.Web.Bitrate = 22000
	next.VNC.Bitrate = 19000
	if reasons := restartReasons(previous, next); len(reasons) != 0 {
		t.Fatalf("bitrate changes require full server restart: %v", reasons)
	}
}
