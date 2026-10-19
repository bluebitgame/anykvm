package app

import (
	"gopkg.in/yaml.v3"
	"testing"
)

func TestWebJPEGQualityConfig(t *testing.T) {
	cfg := DefaultConfig()
	if err := yaml.Unmarshal([]byte("web: {http: ':8080', jpeg_quality: 75}"), &cfg); err != nil {
		t.Fatal(err)
	}
	data, err := minimalConfigYAML(cfg)
	if err != nil {
		t.Fatal(err)
	}
	loaded := DefaultConfig()
	if err := yaml.Unmarshal(data, &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.Web.JPEGQuality != 75 {
		t.Fatal("JPEG quality lost after save")
	}
	previous := cfg
	previous.Web.JPEGQuality = 0
	if reasons := restartReasons(previous, cfg); len(reasons) != 0 {
		t.Fatalf("JPEG quality should not require restart: %v", reasons)
	}
	for _, quality := range []int{-1, 101} {
		cfg.Web.JPEGQuality = quality
		if validateConfig(cfg) == nil {
			t.Fatalf("accepted invalid quality %d", quality)
		}
	}
}
