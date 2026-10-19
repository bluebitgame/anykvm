package app

import (
	"testing"

	"anykvm/internal/configpolicy"
)

func checkCurrentServerConfig(data []byte) error {
	type currentConfig Config
	var cfg currentConfig
	return configpolicy.Check(data, &cfg)
}

func TestConfigWritingPolicy(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		bad  bool
	}{
		{"nested", "# RDP listener\nrdp: {addr: ':3389'}\n", false},
		{"source_keys", "targets: [{video: {source: screen}, audio: {source: auto}, hid: {device: auto}}]\n", false},
		{"legacy_video_device", "targets: [{video: {device: screen}}]\n", true},
		{"legacy_audio_device", "targets: [{audio: {device: auto}}]\n", true},
		{"legacy_rdp", "rdp_addr: ':3389'\n", true},
		{"legacy_vnc", "vnc_tls: true\n", true},
		{"legacy_gamestream", "gamestream_state: state.json\n", true},
		{"legacy_machines", "machines: [{name: Desktop}]\n", true},
		{"unknown_option", "rdp: {adr: ':3389'}\n", true},
		{"clipboard_options", "rdp: {clipboard_options: {text: 3, richtext: 0, images: 0, files: 0}}\n", false},
		{"obsolete_clipboard_direction", "rdp: {clipboard_options: {direction: 3}}\n", true},
		{"non_english_header", "# \u6d4b\u8bd5\nrdp: {addr: ':3389'}\n", true},
		{"non_english_inline", "rdp: {addr: ':3389'} # \u6d4b\u8bd5\n", true},
		{"localized_value", "targets: [{name: '\u684c\u9762 # 1'}]\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkCurrentServerConfig([]byte(tc.data))
			if (err != nil) != tc.bad {
				t.Fatalf("policy error = %v; want rejection = %t", err, tc.bad)
			}
		})
	}
}
