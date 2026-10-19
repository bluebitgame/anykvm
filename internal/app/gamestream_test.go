package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"anykvm/internal/gamestream"

	"gopkg.in/yaml.v3"
)

func TestGameStreamStatePaths(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, state, want string
	}{
		{"default", "", filepath.Join(filepath.Dir(executable), "state.json")},
		{"relative", "build/test/state.json", "build/test/state.json"},
		{"absolute", filepath.Join(t.TempDir(), "state.json"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.GameStream.Addr = ":47989"
			cfg.GameStream.State = tc.state
			want := tc.want
			if tc.name == "absolute" {
				want = tc.state
			}
			g, err := gamestreamConfig(cfg, false)
			if err != nil {
				t.Fatal(err)
			}
			if g.StateFile != want {
				t.Fatalf("state path = %q, want %q", g.StateFile, want)
			}
		})
	}
}

func TestGameStreamConfig(t *testing.T) {
	cfg := DefaultConfig()
	cfg.GameStream.Addr = "[::1]:48989"
	cfg.GameStream.PacingMbps = 100
	cfg.GameStream.State = filepath.Join(t.TempDir(), "state.json")
	g, err := gamestreamConfig(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if g.EnableAV1 || cfg.Web.Codecs.AV1 || g.EnableHEVC || g.VideoPacingMbps != 100 || g.HTTPSAddr != "[::1]:48984" || g.StateFile != cfg.GameStream.State || g.RequestPIN != nil || g.Machines[0] != "Default" {
		t.Fatalf("unexpected config: %+v", g)
	}
	for _, addr := range []string{":0", ":65515", "localhost", ":https", ":1028"} {
		cfg.GameStream.Addr = addr
		if _, err := gamestreamConfig(cfg, false); err == nil {
			t.Fatalf("accepted address %q", addr)
		}
	}
}

func TestGameStreamYAMLRoundTrip(t *testing.T) {
	cfg := DefaultConfig()
	cfg.GameStream.Addr = ":47989"
	cfg.GameStream.State = "/tmp/gamestream-state.json"
	cfg.GameStream.PacingMbps = 200
	cfg.GameStream.Codecs.HEVC = true
	fp := strings.Repeat("ab", 32)
	cfg.GameStream.ClientOS = map[string]string{fp: "macos"}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil || !got.GameStream.Codecs.HEVC || got.GameStream.PacingMbps != 200 || got.GameStream.Addr != cfg.GameStream.Addr || got.GameStream.State != cfg.GameStream.State {
		t.Fatalf("YAML round trip: %+v %v", got, err)
	}
	if got.GameStream.ClientOS[fp] != "macos" {
		t.Fatalf("client OS override lost in YAML round trip: %v", got.GameStream.ClientOS)
	}
	g, err := gamestreamConfig(got, false)
	if err != nil || g.ClientOS[fp] != "macos" {
		t.Fatalf("client OS override lost in runtime config: %v, %v", g.ClientOS, err)
	}
}

func TestGameStreamClientOSConfigValidation(t *testing.T) {
	for _, values := range []map[string]string{
		{strings.Repeat("ab", 32): "macos"},
		{"not-a-fingerprint": "macos"},
		{strings.Repeat("ab", 32): "unsupported"},
	} {
		cfg := DefaultConfig()
		cfg.GameStream.Addr = ":47989"
		cfg.GameStream.ClientOS = values
		wantValid := values[strings.Repeat("ab", 32)] == "macos"
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := SaveConfig(path, cfg); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); (err == nil) != wantValid {
			t.Fatalf("LoadConfig validity = %v for %v", err, values)
		}
		if _, err := gamestreamConfig(cfg, false); (err == nil) != wantValid {
			t.Fatalf("runtime config validity = %v for %v", err, values)
		}
	}
	// Preserve the nested schema: client_os must never become a top-level field.
	cfg := DefaultConfig()
	cfg.GameStream.ClientOS = map[string]string{strings.Repeat("ab", 32): "macos"}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := yaml.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["client_os"]; exists {
		t.Fatal("client_os was written at the top level")
	}
	if _, exists := fields["gamestream"].(map[string]any)["client_os"]; !exists {
		t.Fatal("gamestream.client_os was not written")
	}
}

func TestGameStreamOptionalCodecs(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		cfg := DefaultConfig()
		cfg.GameStream.Addr = ":47989"
		cfg.GameStream.Codecs.HEVC = enabled
		g, err := gamestreamConfig(cfg, false)
		if err != nil || g.EnableHEVC != enabled {
			t.Fatalf("enable=%v: %+v %v", enabled, g, err)
		}
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := SaveConfig(path, cfg); err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadConfig(path)
		if err != nil || loaded.GameStream.Codecs.HEVC != enabled {
			t.Fatalf("round trip: %+v %v", loaded, err)
		}
	}
}

func TestGameStreamConfigAudioPerMachine(t *testing.T) {
	cfg := DefaultConfig()
	cfg.GameStream.Addr = ":47989"
	cfg.Targets = []TargetConfig{
		{Name: "silent"},
		{Name: "audio", Audio: AudioConfig{Source: "default"}},
		{Name: "silent2"},
		{Name: "USB", Video: VideoConfig{Source: "ios-usb"}, Audio: AudioConfig{Source: "auto"}},
		{Name: "CoreDevice", Video: VideoConfig{Source: "coredevice"}, Audio: AudioConfig{Source: "auto"}},
		{Name: "RTSP", Video: VideoConfig{Source: "rtsp://camera/live"}, Audio: AudioConfig{Source: "auto"}},
		{Name: "RDP", Video: VideoConfig{Source: "rdp://desktop"}, Audio: AudioConfig{Source: "auto"}},
	}
	g, err := gamestreamConfig(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.MachineAudio) != 7 || g.MachineAudio[0] || !g.MachineAudio[1] || g.MachineAudio[2] || !g.MachineAudio[3] || !g.MachineAudio[4] || !g.MachineAudio[5] || !g.MachineAudio[6] {
		t.Fatalf("audio configuration does not match targets: %v", g.MachineAudio)
	}
}

func TestGameStreamConfigPlatformsPerMachine(t *testing.T) {
	cfg := DefaultConfig()
	cfg.GameStream.Addr = ":47989"
	cfg.Targets = []TargetConfig{
		{Name: "local", Video: VideoConfig{Source: "screen"}},
		{Name: "capture card", Video: VideoConfig{Source: "/dev/video0"}},
		{Name: "second display", Video: VideoConfig{Source: "screen:1"}},
		{Name: "phone", Video: VideoConfig{Source: "ios-usb:iPhone"}},
		{Name: "default iPhone", Video: VideoConfig{Source: "ios-usb"}},
		{Name: "direct iPhone", Video: VideoConfig{Source: "coredevice:test-phone"}},
		{Name: "default Android", Video: VideoConfig{Source: "adb"}},
		{Name: "Android", Video: VideoConfig{Source: "adb:emulator-5554"}},
		{Name: "unknown", Video: VideoConfig{Source: "screen capture card"}},
	}
	g, err := gamestreamConfig(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{gamestream.ScreenPlatform(), "", gamestream.ScreenPlatform(), "ios", "ios", "ios", "android", "android", ""}
	if len(g.MachinePlatforms) != len(want) {
		t.Fatalf("platforms do not match targets: %v", g.MachinePlatforms)
	}
	for i, platform := range want {
		if g.MachinePlatforms[i] != platform {
			t.Fatalf("machine %q: platform = %q, want %q", cfg.Targets[i].Name, g.MachinePlatforms[i], platform)
		}
	}
}

func TestAV1FlagsIndependentAndPersisted(t *testing.T) {
	for _, gameStream := range []bool{false, true} {
		for _, web := range []bool{false, true} {
			cfg := DefaultConfig()
			cfg.GameStream.Addr = ":47989"
			cfg.GameStream.Codecs.AV1 = gameStream
			cfg.Web.Codecs.AV1 = web
			g, err := gamestreamConfig(cfg, false)
			if err != nil || g.EnableAV1 != gameStream || g.EnableHEVC {
				t.Fatalf("%+v %v", g, err)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := SaveConfig(path, cfg); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadConfig(path)
			if err != nil || loaded.GameStream.Codecs.AV1 != gameStream || loaded.Web.Codecs.AV1 != web {
				t.Fatalf("%+v %v", loaded, err)
			}
		}
	}
}

func TestGameStreamLowLatencyRateControl(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		enabled     bool
	}{
		{"omitted", "", true},
		{"enabled", "  lowdelay: true\n", true},
		{"disabled", "  lowdelay: false\n", false},
		{"legacy", "  enable_low_latency_rate_control: false\n", false},
		{"precedence", "  enable_low_latency_rate_control: false\n  lowdelay: true\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("gamestream:\n  addr: ':47989'\n"+tc.field), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.GameStream.LowDelay != tc.enabled {
				t.Fatalf("enabled=%v, want %v", cfg.GameStream.LowDelay, tc.enabled)
			}
			g, err := gamestreamConfig(cfg, false)
			if err != nil {
				t.Fatal(err)
			}
			if g.DisableLowLatencyRateControl != !tc.enabled {
				t.Fatal("encoder setting was not propagated")
			}
			if err := SaveConfig(path, cfg); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadConfig(path)
			if err != nil || loaded.GameStream.LowDelay != tc.enabled {
				t.Fatalf("round trip lost setting: %v", err)
			}
		})
	}
}

func TestGameStreamH264Profile(t *testing.T) {
	for _, profile := range []string{"", "constrained_baseline", "baseline", "main", "high", "invalid"} {
		t.Run(profile, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			body := "gamestream:\n  addr: ':47989'\n"
			if profile != "" {
				body += "  profile: " + profile + "\n"
			}
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if profile == "invalid" {
				if err == nil {
					t.Fatal("invalid profile accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := profile
			if want == "" {
				want = "constrained_baseline"
			}
			g, err := gamestreamConfig(cfg, false)
			if err != nil || g.H264Profile != want {
				t.Fatalf("profile not propagated: %q %v", g.H264Profile, err)
			}
			if err := SaveConfig(path, cfg); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadConfig(path)
			if err != nil || loaded.GameStream.Profile != want {
				t.Fatalf("profile round trip failed: %v", err)
			}
		})
	}
}
