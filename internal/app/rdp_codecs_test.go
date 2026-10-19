package app

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRDPAllowedCodecsConfig(t *testing.T) {
	cfg := DefaultConfig()
	if err := yaml.Unmarshal([]byte("rdp: {codecs: {h264: false, planar: false}}"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.RDP.Codecs.H264 || cfg.RDP.Codecs.Planar || !cfg.RDP.Codecs.Progressive || !cfg.RDP.Codecs.ClearCodec {
		t.Fatalf("incorrect allowed codecs: %+v", cfg.RDP.Codecs)
	}
	// Enable the service so its non-default policy is written.
	cfg.RDP.Addr = ":3389"
	data, err := minimalConfigYAML(cfg)
	if err != nil {
		t.Fatal(err)
	}
	loaded := DefaultConfig()
	if err := yaml.Unmarshal(data, &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.RDP.Codecs != cfg.RDP.Codecs {
		t.Fatal("allowed codec policy changed after save")
	}
	if err := yaml.Unmarshal([]byte("rdp: {codec: auto}"), &loaded); err == nil {
		t.Fatal("removed forced-codec field accepted")
	}
}
