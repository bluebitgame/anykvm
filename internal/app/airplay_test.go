package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"anykvm/internal/airplay"
)

func TestAirPlayCaptureConfigAndWebTest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	wire := []byte("web: {http: ':8088'}\nvnc: {addr: ''}\ntargets:\n  - name: iPhone\n    video: {source: 'airplay:Test Receiver', mode: '@30'}\n    airplay: {addr: ':7100', interface: en0}\n")
	if err := os.WriteFile(path, wire, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Targets[0].AirPlay.Addr != ":7100" || cfg.Targets[0].AirPlay.Interface != "en0" || cfg.VNC.Addr != "" || cfg.Web.HTTP != ":8088" {
		t.Fatal(cfg)
	}
	dev, android := newCaptureSession(cfg.Targets[0])
	defer dev.Close()
	if _, ok := dev.(*airplay.Device); !ok || android != nil {
		t.Fatal("wrong capture backend", dev)
	}
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadConfig(path)
	if err != nil || reloaded.Targets[0].AirPlay != cfg.Targets[0].AirPlay {
		t.Fatal(reloaded, err)
	}
	// An omitted HID must not auto-detect serial ports or inject input locally.
	machines := buildMachines(cfg)
	if len(machines) != 1 || machines[0].HID != nil || machines[0].AudioDev == nil {
		t.Fatal("AirPlay must provide built-in audio without implicit HID")
	}
	for _, m := range machines {
		m.Capture.Close()
	}
	for _, bad := range []string{
		"video: {source: 'airplay:'}\n",
		"video: {source: airplay}\nairplay: {addr: ':0'}\n",
		"video: {source: airplay}\nairplay: {unknown: true}\n",
	} {
		data := "targets:\n  - name: bad\n    " + strings.ReplaceAll(strings.TrimSuffix(bad, "\n"), "\n", "\n    ") + "\n"
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Fatal("invalid AirPlay config accepted", bad)
		}
	}
}

func TestAirPlayGameStreamArtwork(t *testing.T) {
	cfg := DefaultConfig()
	cfg.GameStream.Addr = ":47989"
	cfg.Targets = []TargetConfig{{Name: "Phone", Video: VideoConfig{Source: "airplay:Receiver"}, Audio: AudioConfig{Source: "auto"}}}
	g, err := gamestreamConfig(cfg, false)
	if err != nil || len(g.MachinePlatforms) != 1 || g.MachinePlatforms[0] != "ios" || !g.MachineAudio[0] {
		t.Fatal(g, err)
	}
}
