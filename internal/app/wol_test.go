package app

import (
	"path/filepath"
	"testing"

	"anykvm/internal/vnc"
	"anykvm/internal/wol"
)

func TestTargetWoLRoundTrip(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Targets[0].Video.Source = "vnc://127.0.0.1:5900"
	cfg.Targets[0].WoL = wol.Options{MAC: "02:11:22:33:44:55", Broadcast: "192.168.1.255:9"}
	for _, save := range []func(string, Config) error{SaveConfig, SaveWebConfig} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := save(path, cfg); err != nil {
			t.Fatal(err)
		}
		got, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if got.Targets[0].WoL != cfg.Targets[0].WoL {
			t.Fatalf("lost WoL: %+v", got.Targets[0])
		}
		dev, _ := newCaptureSession(got.Targets[0])
		if _, ok := dev.(*vnc.VNCClient); !ok {
			t.Fatalf("unexpected capture %T", dev)
		}
	}
	cfg.Targets[0].WoL.MAC = "invalid"
	if validateConfig(cfg) == nil {
		t.Fatal("invalid MAC accepted")
	}
}
