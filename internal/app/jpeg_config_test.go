package app

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestJPEGQualityBelongsToService(t *testing.T) {
	cfg := DefaultConfig()
	if err := yaml.Unmarshal([]byte("vnc: {addr: ':5900', jpeg_quality: 85}\ntargets: [{video: {source: screen}}]\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.VNC.JPEGQuality != 85 {
		t.Fatal("service JPEG quality was not retained")
	}
	data, err := yaml.Marshal(cfg.Targets[0].Video)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "jpeg_quality") {
		t.Fatal("capture configuration still exports JPEG quality")
	}
	if err := yaml.Unmarshal([]byte("targets: [{video: {jpeg_quality: 80}}]"), &cfg); err == nil || !strings.Contains(err.Error(), "video.jpeg_quality was removed") {
		t.Fatalf("removed capture JPEG quality was not rejected: %v", err)
	}
}
