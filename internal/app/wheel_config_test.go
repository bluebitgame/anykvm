package app

import (
	"math"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWheelScaleConfig(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  float64
	}{
		{"", 1}, {", wheel_scale: 2", 2}, {", wheel_scale: -0.5", -.5}, {", wheel_scale: 0", 0},
	} {
		cfg := DefaultConfig()
		if err := yaml.Unmarshal([]byte("targets: [{name: test, video: {source: screen}, hid: {device: local"+tc.value+"}}]"), &cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Targets[0].HID.WheelScale != tc.want {
			t.Fatalf("scale=%v, want %v", cfg.Targets[0].HID.WheelScale, tc.want)
		}
		data, err := minimalConfigYAML(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if tc.want == 1 && strings.Contains(string(data), "wheel_scale") {
			t.Fatal("default wheel scale was not omitted")
		}
		reloaded := DefaultConfig()
		if err := yaml.Unmarshal(data, &reloaded); err != nil {
			t.Fatal(err)
		}
		if reloaded.Targets[0].HID.WheelScale != tc.want {
			t.Fatal("wheel scale changed after saving")
		}
	}
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		cfg := DefaultConfig()
		cfg.Targets[0].HID.WheelScale = value
		if err := validateConfig(cfg); err == nil {
			t.Fatal("non-finite scale accepted")
		}
	}
}
