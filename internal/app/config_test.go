package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"anykvm/internal/clipboard"

	"gopkg.in/yaml.v3"
)

func TestLoadConfigMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.yaml")
	_, err := LoadConfig(path)
	if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), path) {
		t.Fatalf("missing config must return its path and a not-exist error: %v", err)
	}
}

func TestWebBufferFramesConfig(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		frames     int
		invalid    bool
	}{
		{"default", "{}", 0, false},
		{"web", "web: {buffer_frames: 8}", 8, false},
		{"zero", "web: {buffer_frames: 0}", 0, false},
		{"negative", "web: {buffer_frames: -1}", 0, true},
		{"too large", "web: {buffer_frames: 9}", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "buffer_frames") {
					t.Fatalf("expected queue validation error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Web.BufferFrames != tc.frames {
				t.Fatalf("unexpected queue: %+v", cfg.Web)
			}
			if err := SaveConfig(path, cfg); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadConfig(path)
			if err != nil || loaded.Web != cfg.Web {
				t.Fatalf("queue settings did not round-trip: %v", err)
			}
		})
	}
}

func TestDisableAWDLConfig(t *testing.T) {
	for _, example := range []struct {
		name, data string
		want       bool
	}{
		{"default", "{}", false},
		{"enabled", "disable_awdl: true", true},
		{"disabled", "disable_awdl: false", false},
	} {
		t.Run(example.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(example.data), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil || cfg.DisableAWDL != example.want {
				t.Fatalf("disable_awdl=%t, want %t, err=%v", cfg.DisableAWDL, example.want, err)
			}
			if err := SaveConfig(path, cfg); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadConfig(path)
			if err != nil || loaded.DisableAWDL != example.want {
				t.Fatalf("saved disable_awdl changed: %v", err)
			}
		})
	}
}

func TestProtocolConfigFormats(t *testing.T) {
	want := DefaultConfig()
	want.VNC = VNCConfig{Codecs: want.VNC.Codecs, Bitrate: 12000, Addr: ":5901", Password: "test-password", TLS: true, PixelTol: 9, JPEGQuality: 75, ForceCont: true}
	want.Web.HTTP = ":8080"
	want.Web.HTTPS = ":8443"
	want.Web.Username = "anykvm"
	want.Web.Password = "test-password"
	want.RDP.Addr = ":3389"
	want.GameStream = GameStreamConfig{
		Addr: ":47989", State: "/tmp/state.json", PacingMbps: 100,
		Profile: want.GameStream.Profile, LowDelay: want.GameStream.LowDelay,
		Codecs: GameStreamCodecsConfig{HEVC: true, AV1: true},
	}
	want.TLSCert, want.TLSKey = "cert.pem", "key.pem"
	want.Web.Codecs.AV1 = true
	shared := `
tls_cert: cert.pem
tls_key: key.pem
`
	for name, data := range map[string]string{
		"nested": `
vnc:
  addr: ":5901"
  password: test-password
  tls: true
  pixel_tolerance: 9
  jpeg_quality: 75
  force_continuous: true
web:
  http: ":8080"
  https: ":8443"
  username: anykvm
  password: test-password
  codecs:
    av1: true
rdp:
  addr: ":3389"
gamestream:
  addr: ":47989"
  state: /tmp/state.json
  pacing_mbps: 100
  codecs:
    hevc: true
    av1: true
`,
		"legacy": `
vnc_addr: ":5901"
vnc_tls: true
pixel_tolerance: 9
jpeg_quality: 75
force_continuous: true
http_addr: ":8080"
https_addr: ":8443"
rdp_addr: ":3389"
gamestream_addr: ":47989"
gamestream_state: /tmp/state.json
gamestream_pacing_mbps: 100
gamestream_codecs:
  hevc: true
  av1: true
password: test-password
web:
  username: anykvm
  password: test-password
  codecs:
    av1: true
`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(data+shared), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatal("protocol settings or omitted defaults were not preserved")
			}
			if err := SaveConfig(path, got); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadConfig(path)
			if err != nil || !reflect.DeepEqual(loaded, want) {
				t.Fatalf("protocol settings changed after saving: %v", err)
			}
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]any
			if err := yaml.Unmarshal(saved, &document); err != nil {
				t.Fatal(err)
			}
			for protocol, addr := range map[string]string{
				"vnc": want.VNC.Addr, "rdp": want.RDP.Addr, "gamestream": want.GameStream.Addr,
			} {
				block, ok := document[protocol].(map[string]any)
				if !ok || block["addr"] != addr {
					t.Fatalf("saved %s address is not in its protocol block", protocol)
				}
			}
			web, ok := document["web"].(map[string]any)
			if !ok || web["http"] != want.Web.HTTP || web["https"] != want.Web.HTTPS || web["username"] != want.Web.Username || web["password"] != want.Web.Password {
				t.Fatal("saved Web settings are not in the shared web block")
			}
			for _, key := range []string{
				"vnc_addr", "vnc_tls", "pixel_tolerance", "jpeg_quality", "force_continuous",
				"http_addr", "https_addr", "rdp_addr", "gamestream_addr", "gamestream_state",
				"gamestream_pacing_mbps", "gamestream_codecs",
			} {
				if _, ok := document[key]; ok {
					t.Fatalf("saved config contains legacy key %q", key)
				}
			}
		})
	}
}

func TestWebCodecsUseNestedBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("web:\n  codecs:\n    hevc: false\n    av1: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil || cfg.Web.Codecs.HEVC || !cfg.Web.Codecs.AV1 {
		t.Fatalf("nested Web codecs were not loaded: %+v, %v", cfg.Web.Codecs, err)
	}
	if err := os.WriteFile(path, []byte("web_codecs:\n  av1: true\nweb_username: old-user\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(path)
	if err != nil || cfg.Web.Codecs.AV1 || cfg.Web.Username != "" {
		t.Fatalf("removed Web compatibility keys changed config: %+v, %v", cfg.Web, err)
	}
}

func TestVNCPasswordConfiguration(t *testing.T) {
	for name, tc := range map[string]struct {
		data    string
		wantVNC string
		wantWeb string
	}{
		"independent": {
			data:    "vnc: {addr: ':5900', password: vnc-secret}\nweb: {password: web-secret}\n",
			wantVNC: "vnc-secret",
			wantWeb: "web-secret",
		},
		"legacy shared": {
			data:    "vnc: {addr: ':5900'}\npassword: legacy-secret\n",
			wantVNC: "legacy-secret",
			wantWeb: "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.VNC.Password != tc.wantVNC || cfg.Web.Password != tc.wantWeb {
				t.Fatalf("passwords = VNC %q, Web %q; want VNC %q, Web %q", cfg.VNC.Password, cfg.Web.Password, tc.wantVNC, tc.wantWeb)
			}
		})
	}
}

func TestProtocolConfigNestedPrecedence(t *testing.T) {
	legacy := `
vnc_addr: ":5901"
vnc_tls: true
pixel_tolerance: 9
jpeg_quality: 75
force_continuous: true
http_addr: ":8080"
https_addr: ":8443"
rdp_addr: ":3389"
gamestream_addr: ":47989"
gamestream_state: /tmp/state.json
gamestream_pacing_mbps: 100
gamestream_codecs:
  hevc: true
  av1: true
`
	nested := `
vnc:
  addr: ""
  tls: false
  pixel_tolerance: 0
  jpeg_quality: 0
  force_continuous: false
web:
  http: ":8081"
  https: ""
rdp:
  addr: ""
gamestream:
  addr: ""
  state: ""
  pacing_mbps: 0
  codecs:
    av1: false
`
	want := DefaultConfig()
	want.Web.HTTP = ":8081"
	want.GameStream.Codecs.HEVC = true // An omitted nested field keeps its legacy value.
	for name, data := range map[string]string{"nested_first": nested + legacy, "legacy_first": legacy + nested} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := LoadConfig(path)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("nested values must win regardless of key order: %v", err)
			}
		})
	}
}

func TestRDPConfigRoundTrip(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RDP = RDPConfig{Addr: "127.0.0.1:3389", Username: "test", Password: "secret", Bitrate: 8000, Codecs: RDPCodecsConfig{Progressive: true}, ProgressiveQuantization: 8, ProgressiveRefinement: 2}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil || got.RDP != cfg.RDP {
		t.Fatalf("RDP config roundtrip: %v", err)
	}
	if DefaultConfig().RDP.Addr != "" {
		t.Fatal("RDP must default to disabled")
	}
	if DefaultConfig().RDP.Bitrate != 12000 {
		t.Fatal("RDP must default to 12000 kbps")
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("RDP credentials must be saved privately")
	}
}

func TestRDPClipboardConfig(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		want       bool
	}{
		{"omitted-block", "vnc: {addr: ':5900'}\n", true},
		{"omitted-key", "rdp: {addr: ':3389'}\n", true},
		{"empty-block", "rdp: {}\n", true},
		{"enabled", "rdp: {clipboard: true}\n", true},
		{"disabled", "rdp: {clipboard: false}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil || cfg.RDP.Clipboard != tc.want {
				t.Fatalf("clipboard=%t want=%t: %v", cfg.RDP.Clipboard, tc.want, err)
			}
			if err := SaveConfig(path, cfg); err != nil {
				t.Fatal(err)
			}
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var document struct {
				RDP map[string]any `yaml:"rdp"`
			}
			if err := yaml.Unmarshal(saved, &document); err != nil {
				t.Fatal(err)
			}
			if got, ok := document.RDP["clipboard"]; !ok || got != tc.want {
				t.Fatal("saved config omitted or changed the nested clipboard option")
			}
			reloaded, err := LoadConfig(path)
			if err != nil || reloaded.RDP.Clipboard != tc.want {
				t.Fatal("clipboard option did not survive save/reload", err)
			}
		})
	}
	data, err := defaultConfigYAML()
	if err != nil {
		t.Fatal(err)
	}
	var reference Config
	if err := yaml.Unmarshal(data, &reference); err != nil || !reference.RDP.Clipboard {
		t.Fatal("generated reference config must enable clipboard sharing", err)
	}
}

func TestRDPClipboardOptionsConfig(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		want       clipboard.Options
	}{
		{"omitted-block", "{}", clipboard.DefaultOptions()},
		{"omitted-options", "rdp: {}", clipboard.DefaultOptions()},
		{"empty-options", "rdp: {clipboard_options: {}}", clipboard.DefaultOptions()},
		{"partial-options", "rdp: {clipboard_options: {files: 1}}", clipboard.Options{Text: 3, RichText: 3, Images: 3, Files: 1}},
		{"text-off", "rdp: {clipboard_options: {text: 0}}", clipboard.Options{RichText: 3, Images: 3, Files: 3}},
		{"richtext-off", "rdp: {clipboard_options: {richtext: 0}}", clipboard.Options{Text: 3, Images: 3, Files: 3}},
		{"images-off", "rdp: {clipboard_options: {images: 0}}", clipboard.Options{Text: 3, RichText: 3, Files: 3}},
		{"files-off", "rdp: {clipboard_options: {files: 0}}", clipboard.Options{Text: 3, RichText: 3, Images: 3}},
		{"all-off", "rdp: {clipboard_options: {text: 0, richtext: 0, images: 0, files: 0}}", clipboard.Options{}},
		{"mixed", "rdp: {clipboard_options: {text: 1, richtext: 2, images: 3, files: 0}}", clipboard.Options{Text: 1, RichText: 2, Images: 3}},
		{"master-off", "rdp: {clipboard: false, clipboard_options: {text: 2, richtext: 3, images: 1, files: 2}}", clipboard.Options{Text: 2, RichText: 3, Images: 1, Files: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil || cfg.RDP.ClipboardOptions != tc.want {
				t.Fatalf("clipboard_options=%+v want=%+v: %v", cfg.RDP.ClipboardOptions, tc.want, err)
			}
			if err := SaveConfig(path, cfg); err != nil {
				t.Fatal(err)
			}
			reloaded, err := LoadConfig(path)
			if err != nil || reloaded.RDP != cfg.RDP {
				t.Fatalf("clipboard options did not survive save/reload: %v", err)
			}
		})
	}
	data, err := defaultConfigYAML()
	if err != nil {
		t.Fatal(err)
	}
	var reference struct {
		RDP struct {
			Options map[string]int `yaml:"clipboard_options"`
		} `yaml:"rdp"`
	}
	if err := yaml.Unmarshal(data, &reference); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reference.RDP.Options, map[string]int{"text": 3, "richtext": 3, "images": 3, "files": 3}) {
		t.Fatal("generated reference must include all clipboard types with their defaults")
	}
}

func TestRDPClipboardOptionsInvalid(t *testing.T) {
	for _, field := range []string{"text", "richtext", "images", "files"} {
		for _, value := range []string{"-1", "4", "1.5", "1.0", "true", "'1'"} {
			t.Run(field+"="+value, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.yaml")
				data := fmt.Sprintf("rdp: {clipboard_options: {%s: %s}}", field, value)
				if err := os.WriteFile(path, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := LoadConfig(path); err == nil {
					t.Fatal("accepted a clipboard direction outside integer 0..3")
				}
			})
		}
	}
}

func TestRDPAutoShortcutMappingConfig(t *testing.T) {
	if !DefaultConfig().RDP.AutoShortcutMapping {
		t.Fatal("RDP shortcut mapping must default to enabled")
	}
	for _, tc := range []struct {
		name, data string
		want       bool
	}{
		{"omitted-block", "vnc: {addr: ':5900'}\n", true},
		{"omitted-key", "rdp: {addr: ':3389'}\n", true},
		{"empty-block", "rdp: {}\n", true},
		{"enabled", "rdp: {auto_shortcut_mapping: true}\n", true},
		{"disabled", "rdp: {auto_shortcut_mapping: false}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil || cfg.RDP.AutoShortcutMapping != tc.want {
				t.Fatalf("auto_shortcut_mapping=%t want=%t: %v", cfg.RDP.AutoShortcutMapping, tc.want, err)
			}
			if err := SaveConfig(path, cfg); err != nil {
				t.Fatal(err)
			}
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var document struct {
				RDP map[string]any `yaml:"rdp"`
			}
			if err := yaml.Unmarshal(saved, &document); err != nil {
				t.Fatal(err)
			}
			if got, ok := document.RDP["auto_shortcut_mapping"]; !ok || got != tc.want {
				t.Fatal("saved config omitted or changed the nested shortcut mapping option")
			}
			reloaded, err := LoadConfig(path)
			if err != nil || reloaded.RDP.AutoShortcutMapping != tc.want {
				t.Fatal("shortcut mapping did not survive save/reload", err)
			}
		})
	}
	data, err := defaultConfigYAML()
	if err != nil {
		t.Fatal(err)
	}
	var reference Config
	if err := yaml.Unmarshal(data, &reference); err != nil || !reference.RDP.AutoShortcutMapping {
		t.Fatal("generated reference config must enable shortcut mapping", err)
	}
}

func TestRDPHEVCConfig(t *testing.T) {
	if DefaultConfig().RDP.Codecs.HEVC {
		t.Fatal("RDP HEVC must default to disabled")
	}
	for _, example := range []struct {
		name, data      string
		rdp, gameStream bool
	}{
		{"omitted", "{}", false, false},
		{"enabled", "rdp:\n  codecs:\n    hevc: true\n", true, false},
		{"disabled", "rdp:\n  codecs:\n    hevc: false\n", false, false},
		{"gamestream_only", "gamestream:\n  codecs:\n    hevc: true\n", false, true},
		{"both", "rdp:\n  codecs:\n    hevc: true\ngamestream:\n  codecs:\n    hevc: true\n", true, true},
	} {
		t.Run(example.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(example.data), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.RDP.Codecs.HEVC != example.rdp || cfg.GameStream.Codecs.HEVC != example.gameStream {
				t.Fatal("RDP and GameStream HEVC options must be independent")
			}
			if err := SaveConfig(path, cfg); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadConfig(path)
			if err != nil || loaded.RDP.Codecs != cfg.RDP.Codecs || loaded.GameStream.Codecs != cfg.GameStream.Codecs {
				t.Fatalf("HEVC options changed after saving: %v", err)
			}
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var document struct {
				RDP struct {
					Codecs map[string]bool `yaml:"codecs"`
				} `yaml:"rdp"`
			}
			if err := yaml.Unmarshal(saved, &document); err != nil {
				t.Fatal(err)
			}
			if hevc, ok := document.RDP.Codecs["hevc"]; !ok || hevc != example.rdp {
				t.Fatal("saved config must explicitly include rdp.codecs.hevc")
			}
		})
	}
}

func TestRDPQuantYAML(t *testing.T) {
	for _, example := range []struct {
		yaml string
		want int
	}{
		{"rdp:\n  codecs: {h264: false, progressive: true, planar: false, clearcodec: false}\n", 6},
		{"rdp:\n  codecs: {h264: false, progressive: true, planar: false, clearcodec: false}\n  quant: 7\n", 7},
		{"rdp:\n  codecs: {h264: false, progressive: true, planar: false, clearcodec: false}\n  quant: 15\n", 15},
		{"rdp:\n  codecs: {h264: false, progressive: true, planar: false, clearcodec: false}\n  quant: 0\n", 0},
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(example.yaml), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(path)
		if err != nil || cfg.RDP.ProgressiveQuantization != example.want {
			t.Fatalf("rdp.quant: got %d, want %d, err=%v", cfg.RDP.ProgressiveQuantization, example.want, err)
		}
		if err := SaveConfig(path, cfg); err != nil {
			t.Fatal(err)
		}
		saved, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(saved), "quant:") {
			t.Fatalf("saved config missing short quant key: %v", err)
		}
	}
}

func TestRDPDriveConfig(t *testing.T) {
	for _, tc := range []struct {
		yaml    string
		want    uint8
		invalid bool
	}{
		{"rdp: {}", 3, false},
		{"rdp: {drive: {auto_mount: true}}", 3, false},
		{"rdp: {drive: {auto_mount: false}}", 3, false},
		{"rdp: {drive: {permissions: 0}}", 0, false},
		{"rdp: {drive: {permissions: 1}}", 1, false},
		{"rdp: {drive: {permissions: 2}}", 2, false},
		{"rdp: {drive: {permissions: 4}}", 0, true},
		{"rdp: {drive: {permissions: -1}}", 0, true},
	} {
		name := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(name, []byte(tc.yaml), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(name)
		if tc.invalid {
			if err == nil {
				t.Fatalf("invalid drive settings accepted: %s", tc.yaml)
			}
			continue
		}
		if err != nil || cfg.RDP.Drive.Permissions != tc.want {
			t.Fatalf("drive settings %s: %v %+v", tc.yaml, err, cfg.RDP.Drive)
		}
		if cfg.RDP.Drive.AutoMount == strings.Contains(tc.yaml, "auto_mount: false") {
			t.Fatal("drive auto_mount must default to true and respect explicit false")
		}
		if err := SaveConfig(name, cfg); err != nil {
			t.Fatal(err)
		}
		got, err := LoadConfig(name)
		if err != nil || got.RDP.Drive != cfg.RDP.Drive {
			t.Fatal("drive round trip", err)
		}
		if reasons := restartReasons(cfg, func() Config { next := cfg; next.RDP.Drive.Permissions ^= 1; return next }()); len(reasons) == 0 {
			t.Fatal("drive permission edits must require reconnect/restart")
		}
		if reasons := restartReasons(cfg, func() Config { next := cfg; next.RDP.Drive.AutoMount = !next.RDP.Drive.AutoMount; return next }()); len(reasons) == 0 {
			t.Fatal("auto_mount edits must require reconnect/restart")
		}
	}
}
