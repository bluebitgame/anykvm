package app

import (
	"testing"

	"anykvm/internal/capture"

	"gopkg.in/yaml.v3"
)

func TestResolutionAutoAndNative(t *testing.T) {
	for _, tc := range []struct {
		size      string
		auto      bool
		w, h, fps int
	}{
		{"", false, 0, 0, 0}, {"@30", false, 0, 0, 30},
		{"auto", true, 0, 0, 0}, {"auto@30", true, 0, 0, 30},
		{" AUTO@60 ", true, 0, 0, 60}, {"1920x1080@60", false, 1920, 1080, 60},
	} {
		w, h, fps := capture.ParseResolution(tc.size)
		if w != tc.w || h != tc.h || fps != tc.fps || capture.ResolutionFollowsClient(tc.size) != tc.auto {
			t.Fatalf("size=%q: %dx%d@%d auto=%t", tc.size, w, h, fps, capture.ResolutionFollowsClient(tc.size))
		}
		cfg := Config{Targets: []TargetConfig{{Video: VideoConfig{Source: "screen:0", Mode: tc.size}}}}
		data, err := yaml.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var restored Config
		if err := yaml.Unmarshal(data, &restored); err != nil {
			t.Fatal(err)
		}
		if restored.Targets[0].Video.Mode != tc.size {
			t.Fatal("resolution policy did not survive config roundtrip")
		}
	}
}
