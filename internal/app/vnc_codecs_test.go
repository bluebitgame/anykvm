package app

import (
	"gopkg.in/yaml.v3"
	"testing"
)

func TestVNCOptionalCodecsConfig(t *testing.T) {
	cfg := DefaultConfig()
	if err := yaml.Unmarshal([]byte("vnc: {addr: ':5900', codecs: {jpeg: false, h264: false}}"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.VNC.Codecs.JPEG || cfg.VNC.Codecs.H264 || !cfg.VNC.Codecs.Tight {
		t.Fatalf("incorrect codec defaults: %+v", cfg.VNC.Codecs)
	}
	data, err := minimalConfigYAML(cfg)
	if err != nil {
		t.Fatal(err)
	}
	loaded := DefaultConfig()
	if err := yaml.Unmarshal(data, &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.VNC.Codecs != cfg.VNC.Codecs {
		t.Fatal("codec policy changed after save")
	}
	if reasons := restartReasons(loaded, cfg); len(reasons) != 0 {
		t.Fatalf("unchanged config requires restart: %v", reasons)
	}
}
