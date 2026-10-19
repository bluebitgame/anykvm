package app

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCaptureSourceWritePolicy(t *testing.T) {
	for _, tc := range []struct{ name, wire, video, audio string }{
		{"current", "video: {source: screen}\naudio: {source: auto}", "screen", "auto"},
		{"empty_source", "video: {source: ''}\naudio: {source: ''}", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var target TargetConfig
			if e := yaml.Unmarshal([]byte(tc.wire), &target); e != nil {
				t.Fatal(e)
			}
			if target.Video.Source != tc.video || target.Audio.Source != tc.audio {
				t.Fatalf("source values changed: %q %q", target.Video.Source, target.Audio.Source)
			}
			wire, e := yaml.Marshal(target)
			if e != nil {
				t.Fatal(e)
			}
			var root map[string]any
			if e = yaml.Unmarshal(wire, &root); e != nil {
				t.Fatal(e)
			}
			for _, key := range []string{"video", "audio"} {
				block := root[key].(map[string]any)
				if _, ok := block["device"]; ok {
					t.Fatalf("wrote legacy %s.device", key)
				}
				if _, ok := block["source"]; !ok {
					t.Fatalf("missing %s.source", key)
				}
			}
		})
	}
}

func TestCaptureSourceRejectsDeviceKeys(t *testing.T) {
	for _, wire := range []string{
		"video: {device: screen}", "audio: {device: auto}",
		"video: {source: screen, device: old}", "audio: {source: auto, device: old}",
		"video: {device: old, source: ''}", "audio: {device: auto, source: ''}",
	} {
		var target TargetConfig
		if err := yaml.Unmarshal([]byte(wire), &target); err == nil {
			t.Fatalf("accepted removed field: %s", wire)
		}
	}
}
