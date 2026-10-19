package app

import (
	"gopkg.in/yaml.v3"
	"testing"
)

func TestWebH264Config(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.Web.Codecs.H264 {
		t.Fatal("H.264 should default to enabled")
	}
	if err := yaml.Unmarshal([]byte("web: {http: ':8080', codecs: {h264: false}}"), &cfg); err != nil {
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
	if loaded.Web.Codecs.H264 || !loaded.Web.Codecs.HEVC {
		t.Fatalf("codec settings changed after save: %+v", loaded.Web.Codecs)
	}
}
