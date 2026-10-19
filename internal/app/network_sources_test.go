package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"anykvm/internal/rdp"
	"anykvm/internal/rtsp"
)

func TestNetworkSourcesURLSelectionAndAutoDevices(t *testing.T) {
	for _, uri := range []string{"rtsp://user:secret@camera/live", "rdp://user:secret@desktop:3389"} {
		t.Run(uri[:4], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if e := os.WriteFile(path, []byte("targets:\n  - name: Remote\n    video: {source: '"+uri+"'}\n"), 0600); e != nil {
				t.Fatal(e)
			}
			cfg, e := LoadConfig(path)
			if e != nil {
				t.Fatal(e)
			}
			machines := buildMachines(cfg)
			if len(machines) != 1 || machines[0].AudioDev == nil {
				t.Fatal("source audio auto not selected")
			}
			switch dev := machines[0].Capture.(type) {
			case *rdp.SourceClient:
				if _, ok := any(dev).(bridge.HIDSender); !ok || machines[0].HID == nil {
					t.Fatal("RDP HID auto not selected")
				}
			case *rtsp.Device:
				if machines[0].HID != nil {
					t.Fatal("RTSP unexpectedly supplies input")
				}
			default:
				t.Fatalf("wrong source %T", dev)
			}
			cfg.Targets[0].RDP.Password = "rdp-block-secret"
			cfg.Targets[0].RTSP.Password = "rtsp-block-secret"
			if e = SaveConfig(path, cfg); e != nil {
				t.Fatal(e)
			}
			info, _ := os.Stat(path)
			if info.Mode().Perm() != 0600 {
				t.Fatal("source password permissions")
			}
			view := webConfigView(cfg)
			data, e := json.Marshal(view)
			if e != nil {
				t.Fatal(e)
			}
			if strings.Contains(string(data), "secret") {
				t.Fatal("source password leaked")
			}
			if cfg.Targets[0].Video.Source != uri {
				t.Fatal("view mutated live source URL")
			}
			view.restoreRetainedSecrets(cfg)
			if view.Targets[0].Video.Source != uri || view.Targets[0].RTSP.Password != "rtsp-block-secret" || view.Targets[0].RDP.Password != "rdp-block-secret" {
				t.Fatal("source credentials were not retained")
			}
		})
	}
}
func TestNetworkSourceValidationPreservesFailure(t *testing.T) {
	for _, uri := range []string{"rdp://host:0", "rdp://host/path", "rtsp://host:99999/live", "rtsp://"} {
		target := TargetConfig{Video: VideoConfig{Source: uri}}
		dev, _ := newCaptureSession(target)
		if e := dev.Open(); e == nil {
			t.Fatalf("invalid source fell through: %q", uri)
		}
		cfg := DefaultConfig()
		cfg.Targets = []TargetConfig{target}
		if e := validateConfig(cfg); e == nil {
			t.Fatalf("invalid source accepted: %q", uri)
		}
	}
}

var _ capture.AudioProvider = (*rdp.SourceClient)(nil)
var _ capture.AudioProvider = (*rtsp.Device)(nil)
var _ bridge.HIDSender = (*rdp.SourceClient)(nil)
