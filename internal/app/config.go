package app

import (
	"fmt"
	"math"
	"os"
	"path/filepath"

	"anykvm/internal/airplay"
	"anykvm/internal/ard"
	"anykvm/internal/capture"
	"anykvm/internal/clipboard"
	"anykvm/internal/coredevice"
	"anykvm/internal/gamestream"
	"anykvm/internal/rdp"
	"anykvm/internal/rtsp"
	"anykvm/internal/scrcpy"
	"anykvm/internal/spice"
	"anykvm/internal/vnc"
	"anykvm/internal/wol"

	"gopkg.in/yaml.v3"
)

// Config holds all AnyKVM configuration.
type Config struct {
	Viewer     bool             `yaml:"viewer"` // start the local GUI when built with the viewer tag
	VNC        VNCConfig        `yaml:"vnc"`
	Web        WebConfig        `yaml:"web"`
	RDP        RDPConfig        `yaml:"rdp"`
	GameStream GameStreamConfig `yaml:"gamestream"`

	TLSCert       string `yaml:"tls_cert"` // shared by VNC, HTTPS, and RDP
	TLSKey        string `yaml:"tls_key"`
	AdminPassword string `yaml:"admin_password"` // protects the Web configuration page
	DisableAWDL   bool   `yaml:"disable_awdl"`   // macOS: suspend AWDL while clients stream

	HotkeyMod string         `yaml:"hotkey_mod"`
	Targets   []TargetConfig `yaml:"targets"`
}

// VNCConfig configures the VNC listener and encoding behavior.
type VNCConfig struct {
	Codecs      vnc.CodecOptions `yaml:"codecs"`
	Bitrate     int              `yaml:"bitrate"` // H.264 video target in kbps; 0 uses the encoder default.
	Addr        string           `yaml:"addr"`
	Password    string           `yaml:"password,omitempty"`
	TLS         bool             `yaml:"tls"` // enable VeNCrypt (X.509) TLS encryption
	PixelTol    int              `yaml:"pixel_tolerance"`
	JPEGQuality int              `yaml:"jpeg_quality"`
	ForceCont   bool             `yaml:"force_continuous"`
}

// WebConfig configures the shared HTTP and HTTPS WebSocket media service.
type WebConfig struct {
	JPEGQuality  int             `yaml:"jpeg_quality"` // JPEG encoder quality (1..100); 0 uses the default.
	Bitrate      int             `yaml:"bitrate"`      // encoded Web video target in kbps; 0 uses the encoder default.
	HTTP         string          `yaml:"http"`
	HTTPS        string          `yaml:"https"`
	Username     string          `yaml:"username,omitempty"`
	Password     string          `yaml:"password,omitempty"`
	BufferFrames int             `yaml:"buffer_frames"` // decoded H.264 display queue, 0=auto (1), 1..8
	Codecs       WebCodecsConfig `yaml:"codecs"`
}

// legacyWebListener accepts the old HTTP/HTTPS listener configuration.
type legacyWebListener struct {
	Addr string `yaml:"addr"`
}

// RDPConfig configures the experimental TLS/RDPGFX server account and encoder.
type RDPConfig struct {
	Drive                   rdp.DriveRedirectionOptions `yaml:"drive"`
	Addr                    string                      `yaml:"addr"`
	Username                string                      `yaml:"username"`
	Password                string                      `yaml:"password"`
	Bitrate                 int                         `yaml:"bitrate"`                 // H.264 bitrate in kbps; default 12000, 0 leaves rate control to the encoder.
	BufferFrames            int                         `yaml:"buffer_frames,omitempty"` // Native macOS H.264 in-flight limit; 0=auto (2), 1..8.
	Clipboard               bool                        `yaml:"clipboard"`               // default true; enables bidirectional clipboard sharing.
	ClipboardOptions        clipboard.Options           `yaml:"clipboard_options"`       // Independent directions for text, rich text, images, and files.
	AutoShortcutMapping     bool                        `yaml:"auto_shortcut_mapping"`   // default true; false disables shortcut conversion for every client.
	Codecs                  RDPCodecsConfig             `yaml:"codecs"`
	ProgressiveQuantization int                         `yaml:"quant"`  // Progressive only: 6..15, 0=6; larger is coarser.
	ProgressiveRefinement   int                         `yaml:"refine"` // default 2; 0=complete tiles; 1..4 AC FIRST bits refined per stable tile.
}

// RDPCodecsConfig controls which implemented encodings clients may negotiate.
type RDPCodecsConfig = rdp.CodecOptions

// GameStreamConfig configures the experimental GameStream listener and encoder.
type GameStreamConfig struct {
	Profile    string                 `yaml:"profile"`  // Native VideoToolbox H.264 profile; default constrained_baseline.
	LowDelay   bool                   `yaml:"lowdelay"` // Native VideoToolbox H.264 only; default true.
	Addr       string                 `yaml:"addr"`
	State      string                 `yaml:"state"` // empty uses state.json beside the executable
	PacingMbps int                    `yaml:"pacing_mbps"`
	Codecs     GameStreamCodecsConfig `yaml:"codecs"`
	ClientOS   map[string]string      `yaml:"client_os,omitempty"` // paired certificate SHA-256 -> client OS
}

// UnmarshalYAML also accepts the legacy rate-control key.
func (cfg *GameStreamConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain GameStreamConfig
	if err := node.Decode((*plain)(cfg)); err != nil {
		return err
	}
	var aliases struct {
		Old     *bool `yaml:"enable_low_latency_rate_control"`
		Current *bool `yaml:"lowdelay"`
	}
	if err := node.Decode(&aliases); err != nil {
		return err
	}
	if aliases.Current == nil && aliases.Old != nil {
		cfg.LowDelay = *aliases.Old
	}
	return nil
}

// WebCodecsConfig controls codecs advertised to browser clients.
// HEVC is enabled by default; AV1 is opt-in.
type WebCodecsConfig struct {
	H264 bool `yaml:"h264"`
	HEVC bool `yaml:"hevc"`
	AV1  bool `yaml:"av1"`
}

// GameStreamCodecsConfig opts into optional codecs. H.264 is always available.
type GameStreamCodecsConfig struct {
	HEVC bool `yaml:"hevc"`
	AV1  bool `yaml:"av1"`
}

// TargetConfig defines one controlled target.
type TargetConfig struct {
	WoL        wol.Options        `yaml:"wol,omitempty"`
	Name       string             `yaml:"name"`
	Video      VideoConfig        `yaml:"video"`
	Audio      AudioConfig        `yaml:"audio"`
	HID        HIDConfig          `yaml:"hid"`
	Scrcpy     ScrcpyConfig       `yaml:"scrcpy"`
	ARD        ard.Options        `yaml:"ard,omitempty"`
	CoreDevice coredevice.Options `yaml:"coredevice,omitempty"`
	AirPlay    airplay.Options    `yaml:"airplay,omitempty"`
	RTSP       rtsp.Options       `yaml:"rtsp,omitempty"`
	SPICE      spice.Options      `yaml:"spice,omitempty"`
	RDP        rdp.SourceOptions  `yaml:"rdp,omitempty"`
}

func (cfg *TargetConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain TargetConfig
	cfg.HID.WheelScale = 1
	cfg.HID.Device = "auto"
	cfg.Audio.Source = "auto"
	return node.Decode((*plain)(cfg))
}

// VideoConfig defines the video capture source.
type VideoConfig struct {
	Source          string `yaml:"source"`
	Mode            string `yaml:"mode"`   // "WxH[@FPS][:format]", "auto[@FPS]" (client), "@FPS" or "" (native)
	CursorRendering string `yaml:"cursor"` // client (default) or remote
}

// UnmarshalYAML rejects removed capture settings instead of silently ignoring them.
func (cfg *VideoConfig) UnmarshalYAML(node *yaml.Node) error {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "device" {
			return fmt.Errorf("video.device was removed; use video.source")
		}
		if node.Content[i].Value == "cursor_rendering" {
			return fmt.Errorf("video.cursor_rendering was removed; use video.cursor")
		}
		if node.Content[i].Value == "jpeg_quality" {
			return fmt.Errorf("video.jpeg_quality was removed; configure JPEG quality in the service instead")
		}
		if node.Content[i].Value == "hide_cursor" {
			return fmt.Errorf("video.hide_cursor was removed; use video.cursor: remote or client")
		}
		if node.Content[i].Value == "size" {
			return fmt.Errorf("video.size was removed; use video.mode")
		}
	}
	type plain VideoConfig
	cfg.CursorRendering = "client"
	return node.Decode((*plain)(cfg))
}

// MarshalYAML writes the effective default explicitly.
func (cfg VideoConfig) MarshalYAML() (interface{}, error) {
	type plain VideoConfig
	if cfg.CursorRendering == "" {
		cfg.CursorRendering = "client"
	}
	return plain(cfg), nil
}

// AudioConfig defines audio capture.
type AudioConfig struct {
	Source string `yaml:"source"` // auto follows video or the local screen's audio; empty disables audio
}

func (cfg *AudioConfig) UnmarshalYAML(node *yaml.Node) error {
	if yamlMapValue(node, "device") != nil {
		return fmt.Errorf("audio.device was removed; use audio.source")
	}
	type plain AudioConfig
	return node.Decode((*plain)(cfg))
}

// HIDConfig defines keyboard/mouse output (device is a single backend spec).
type HIDConfig struct {
	Device     string  `yaml:"device"` // auto follows video or local screen input; empty disables input
	MouseMode  string  `yaml:"mouse_mode"`
	WheelScale float64 `yaml:"wheel_scale"` // default 1; negative reverses scrolling, 0 disables it.
}

func (cfg *HIDConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain HIDConfig
	cfg.WheelScale = 1
	return node.Decode((*plain)(cfg))
}

// ScrcpyConfig defines Android scrcpy settings.
type ScrcpyConfig struct {
	KeepActive            bool   `yaml:"keep_active,omitempty"`
	BitRate               int    `yaml:"bitrate"`
	Server                string `yaml:"server"`
	Version               string `yaml:"version"`
	Crop                  string `yaml:"crop,omitempty"`
	GameStreamOrientation string `yaml:"gamestream_orientation,omitempty"`
}

// DefaultConfig returns a Config with default values.
func DefaultConfig() Config {
	return Config{
		VNC:        VNCConfig{Bitrate: 12000, Codecs: vnc.DefaultCodecOptions()},
		Web:        WebConfig{BufferFrames: 0, Bitrate: 12000, Codecs: WebCodecsConfig{H264: true, HEVC: true}},
		HotkeyMod:  "ctrl+alt",
		GameStream: GameStreamConfig{LowDelay: true, Profile: "constrained_baseline"},
		RDP: RDPConfig{
			Username: "anykvm", Bitrate: 12000, Codecs: rdp.DefaultCodecOptions(), Clipboard: true,
			Drive:            rdp.DriveRedirectionOptions{Permissions: 3, AutoMount: true},
			ClipboardOptions: clipboard.DefaultOptions(), AutoShortcutMapping: true,
			ProgressiveQuantization: 6, ProgressiveRefinement: 2,
		},
		Targets: []TargetConfig{{
			Name:  "Default",
			Video: VideoConfig{CursorRendering: "client"},
			HID:   HIDConfig{Device: "auto", MouseMode: "absolute", WheelScale: 1},
			Audio: AudioConfig{Source: "auto"},
		}},
	}
}

// UnmarshalYAML accepts legacy machines and flat protocol keys, then applies the
// current schema. Explicit targets and nested values win, including empty lists,
// empty addresses, false, and zero.
func (cfg *Config) UnmarshalYAML(node *yaml.Node) error {
	if block := yamlMapValue(node, "rdp"); block != nil && yamlMapValue(block, "codec") != nil {
		return fmt.Errorf("rdp.codec was removed; use rdp.codecs to allow encodings")
	}
	var legacy struct {
		VNC struct {
			Password *string `yaml:"password"`
		} `yaml:"vnc"`
		HTTP                 *legacyWebListener      `yaml:"http"`
		HTTPS                *legacyWebListener      `yaml:"https"`
		Password             *string                 `yaml:"password"`
		Machines             yaml.Node               `yaml:"machines"`
		Targets              yaml.Node               `yaml:"targets"`
		VNCAddr              *string                 `yaml:"vnc_addr"`
		VNCTLS               *bool                   `yaml:"vnc_tls"`
		PixelTol             *int                    `yaml:"pixel_tolerance"`
		JPEGQuality          *int                    `yaml:"jpeg_quality"`
		ForceCont            *bool                   `yaml:"force_continuous"`
		HTTPAddr             *string                 `yaml:"http_addr"`
		HTTPSAddr            *string                 `yaml:"https_addr"`
		RDPAddr              *string                 `yaml:"rdp_addr"`
		GameStreamAddr       *string                 `yaml:"gamestream_addr"`
		GameStreamState      *string                 `yaml:"gamestream_state"`
		GameStreamPacingMbps *int                    `yaml:"gamestream_pacing_mbps"`
		GameStreamCodecs     *GameStreamCodecsConfig `yaml:"gamestream_codecs"`
	}
	if err := node.Decode(&legacy); err != nil {
		return err
	}
	if legacy.Targets.Kind == 0 && legacy.Machines.Kind != 0 {
		if err := legacy.Machines.Decode(&cfg.Targets); err != nil {
			return err
		}
	}
	if legacy.VNCAddr != nil {
		cfg.VNC.Addr = *legacy.VNCAddr
	}
	if legacy.VNCTLS != nil {
		cfg.VNC.TLS = *legacy.VNCTLS
	}
	if legacy.PixelTol != nil {
		cfg.VNC.PixelTol = *legacy.PixelTol
	}
	if legacy.JPEGQuality != nil {
		cfg.VNC.JPEGQuality = *legacy.JPEGQuality
	}
	if legacy.ForceCont != nil {
		cfg.VNC.ForceCont = *legacy.ForceCont
	}
	if legacy.HTTP != nil {
		cfg.Web.HTTP = legacy.HTTP.Addr
	}
	if legacy.HTTPS != nil {
		cfg.Web.HTTPS = legacy.HTTPS.Addr
	}
	if legacy.HTTPAddr != nil {
		cfg.Web.HTTP = *legacy.HTTPAddr
	}
	if legacy.HTTPSAddr != nil {
		cfg.Web.HTTPS = *legacy.HTTPSAddr
	}
	if legacy.RDPAddr != nil {
		cfg.RDP.Addr = *legacy.RDPAddr
	}
	if legacy.GameStreamAddr != nil {
		cfg.GameStream.Addr = *legacy.GameStreamAddr
	}
	if legacy.GameStreamState != nil {
		cfg.GameStream.State = *legacy.GameStreamState
	}
	if legacy.GameStreamPacingMbps != nil {
		cfg.GameStream.PacingMbps = *legacy.GameStreamPacingMbps
	}
	if legacy.GameStreamCodecs != nil {
		cfg.GameStream.Codecs = *legacy.GameStreamCodecs
	}
	type plainConfig Config // avoid recursively calling UnmarshalYAML
	if err := node.Decode((*plainConfig)(cfg)); err != nil {
		return err
	}
	// The legacy top-level password applies only to VNC.
	// Web credentials must be configured under web.
	if legacy.VNC.Password == nil && cfg.VNC.Password == "" && legacy.Password != nil {
		cfg.VNC.Password = *legacy.Password
	}
	return nil
}

// LoadConfig reads a YAML config file and returns a Config.
// Missing or invalid files return an error; startup handles first-run creation.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	if err := validateConfig(cfg); err != nil {
		return cfg, err
	}

	return cfg, nil
}

// validateConfig validates settings without reading or writing configuration.
// Keeping it separate lets the Web configuration endpoint reject invalid edits
// before replacing the active YAML file.
func validateConfig(cfg Config) error {
	if cfg.Web.JPEGQuality < 0 || cfg.Web.JPEGQuality > 100 {
		return fmt.Errorf("web.jpeg_quality must be 0..100")
	}
	if err := cfg.RDP.Drive.Validate(); err != nil {
		return err
	}
	if cfg.VNC.Bitrate < 0 || cfg.VNC.Bitrate > 1000000 {
		return fmt.Errorf("vnc.bitrate must be 0..1000000 kbps")
	}
	if cfg.Web.Bitrate < 0 || cfg.Web.Bitrate > 1000000 {
		return fmt.Errorf("web.bitrate must be 0..1000000 kbps")
	}
	if cfg.RDP.BufferFrames < 0 || cfg.RDP.BufferFrames > 8 {
		return fmt.Errorf("rdp.buffer_frames must be 0..8")
	}
	if err := cfg.RDP.ClipboardOptions.Validate(); err != nil {
		return fmt.Errorf("rdp.clipboard_options.%w", err)
	}
	if cfg.RDP.Addr != "" {
		if err := cfg.RDP.Codecs.Validate(); err != nil {
			return err
		}
	}
	if cfg.Web.BufferFrames < 0 || cfg.Web.BufferFrames > 8 {
		return fmt.Errorf("web.buffer_frames must be 0..8")
	}
	if err := cfg.GameStream.validateProfile(); err != nil {
		return err
	}
	if _, err := gamestream.ParseClientOSOverrides(cfg.GameStream.ClientOS); err != nil {
		return err
	}
	for _, machine := range cfg.Targets {
		if err := machine.WoL.Validate(); err != nil {
			return fmt.Errorf("target %q: %w", machine.Name, err)
		}
		if math.IsNaN(machine.HID.WheelScale) || math.IsInf(machine.HID.WheelScale, 0) {
			return fmt.Errorf("target %q hid.wheel_scale must be finite", machine.Name)
		}
		switch machine.Video.CursorRendering {
		case "", "remote", "client":
		default:
			return fmt.Errorf("target %q video.cursor must be remote or client", machine.Name)
		}
		if _, err := capture.ParseMode(machine.Video.Mode); err != nil {
			return fmt.Errorf("target %q video.mode: %w", machine.Name, err)
		}
		if err := validateAirPlayMachine(machine); err != nil {
			return err
		}
		if err := validateCoreDeviceMachine(machine); err != nil {
			return err
		}
		if err := validateARDMachine(machine); err != nil {
			return err
		}
		if err := rtsp.ValidateSource(machine.Video.Source, machine.RTSP); err != nil {
			return fmt.Errorf("target %q: %w", machine.Name, err)
		}
		if err := spice.ValidateSource(machine.Video.Source, machine.SPICE); err != nil {
			return fmt.Errorf("target %q: %w", machine.Name, err)
		}
		if err := rdp.ValidateSource(machine.Video.Source, machine.RDP); err != nil {
			return fmt.Errorf("target %q: %w", machine.Name, err)
		}
		if err := scrcpy.ValidateCrop(machine.Scrcpy.Crop); err != nil {
			return fmt.Errorf("target %q: scrcpy.crop: %w", machine.Name, err)
		}
		if err := scrcpy.ValidateCaptureOrientation(machine.Scrcpy.GameStreamOrientation); err != nil {
			return fmt.Errorf("target %q: scrcpy.gamestream_orientation: %w", machine.Name, err)
		}
	}

	return nil
}

// SaveConfig writes cfg to path as YAML.
func SaveConfig(path string, cfg Config) error {
	data, err := yaml.Marshal(&cfg)
	if err != nil {
		return err
	}
	return saveConfigData(path, cfg, data)
}

// SaveWebConfig writes the smallest equivalent YAML for the visual editor.
// Disabled services and values inherited from DefaultConfig stay absent, while
// LoadConfig restores those defaults when the file is read again.
func SaveWebConfig(path string, cfg Config) error {
	data, err := minimalConfigYAML(cfg)
	if err != nil {
		return err
	}
	return saveConfigData(path, cfg, data)
}

func saveConfigData(path string, cfg Config, data []byte) error {
	mode := os.FileMode(0644)
	if cfg.Web.Password != "" || cfg.AdminPassword != "" || cfg.VNC.Password != "" || cfg.RDP.Password != "" {
		mode = 0600
	}
	for _, m := range cfg.Targets {
		if m.ARD.Password != "" || m.RTSP.Password != "" || m.RDP.Password != "" || m.SPICE.Password != "" || sourceHasPassword(m.Video.Source) {
			mode = 0600
		}
	}
	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0077 == 0 {
		mode = 0600
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".anykvm-config-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func minimalConfigYAML(cfg Config) ([]byte, error) {
	data, err := yaml.Marshal(&cfg)
	if err != nil {
		return nil, err
	}
	defaults, err := yaml.Marshal(DefaultConfig())
	if err != nil {
		return nil, err
	}
	var actualDoc, defaultDoc yaml.Node
	if err := yaml.Unmarshal(data, &actualDoc); err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(defaults, &defaultDoc); err != nil {
		return nil, err
	}
	if len(actualDoc.Content) == 0 || len(defaultDoc.Content) == 0 {
		return nil, fmt.Errorf("marshal configuration produced an empty document")
	}
	actual := actualDoc.Content[0]
	for _, service := range []string{"vnc", "web", "rdp", "gamestream"} {
		if !serviceEnabled(cfg, service) {
			removeYAMLMapKey(actual, service)
		}
	}
	pruneYAMLDefaults(actual, defaultDoc.Content[0], "")
	return yaml.Marshal(actual)
}

func serviceEnabled(cfg Config, service string) bool {
	switch service {
	case "vnc":
		return cfg.VNC.Addr != ""
	case "web":
		return cfg.Web.HTTP != "" || cfg.Web.HTTPS != ""
	case "rdp":
		return cfg.RDP.Addr != ""
	case "gamestream":
		return cfg.GameStream.Addr != ""
	default:
		return false
	}
}

func removeYAMLMapKey(mapping *yaml.Node, key string) {
	if mapping.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
}

func yamlMapValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func pruneYAMLDefaults(actual, defaults *yaml.Node, field string) bool {
	if actual == nil || defaults == nil || actual.Kind != defaults.Kind {
		return false
	}
	switch actual.Kind {
	case yaml.ScalarNode:
		return actual.Tag == defaults.Tag && actual.Value == defaults.Value
	case yaml.SequenceNode:
		if field == "targets" && len(defaults.Content) > 0 {
			for _, item := range actual.Content {
				pruneYAMLDefaults(item, defaults.Content[0], "")
			}
		}
		if len(actual.Content) != len(defaults.Content) {
			return false
		}
		for i := range actual.Content {
			if !pruneYAMLDefaults(actual.Content[i], defaults.Content[i], "") {
				return false
			}
		}
		return true
	case yaml.MappingNode:
		for i := len(actual.Content) - 2; i >= 0; i -= 2 {
			key, value := actual.Content[i], actual.Content[i+1]
			defaultValue := yamlMapValue(defaults, key.Value)
			if defaultValue != nil && pruneYAMLDefaults(value, defaultValue, key.Value) {
				actual.Content = append(actual.Content[:i], actual.Content[i+2:]...)
			}
		}
		return len(actual.Content) == 0
	default:
		return false
	}
}

// defaultConfigYAML builds the first-run reference with only Web enabled.
// Keep listener defaults out of DefaultConfig so omitted services stay disabled
// when loading an existing configuration.
func defaultConfigYAML() ([]byte, error) {
	cfg := DefaultConfig()
	cfg.Web.HTTP = "127.0.0.1:8080"
	data, err := yaml.Marshal(&cfg)
	if err != nil {
		return nil, err
	}
	header := "# AnyKVM reference configuration\n" +
		"# Web listens on 127.0.0.1:8080 without a password; other services are disabled.\n" +
		"# Configure capture and input devices under targets before connecting.\n"
	return append([]byte(header), data...), nil
}

// SaveDefaultConfig writes the password-free Web reference configuration.
func SaveDefaultConfig(path string) error {
	data, err := defaultConfigYAML()
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func (cfg GameStreamConfig) validateProfile() error {
	return gamestream.ValidateH264Profile(cfg.Profile)
}
