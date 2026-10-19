package app

import (
	"anykvm/internal/capture"
	"encoding/json"
	"gopkg.in/yaml.v3"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVideoModeConfig(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("targets: [{video: {source: Hagibis, mode: '1920x1080@60:nv12'}}]"), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := validateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "mode: 1920x1080@60:nv12") || strings.Contains(string(data), "size:") {
		t.Fatalf("unexpected mode serialization: %s", data)
	}
	for _, wire := range []string{"video: {size: '1920x1080@60'}", "video: {size: '', mode: ''}"} {
		var target TargetConfig
		if err := yaml.Unmarshal([]byte(wire), &target); err == nil || !strings.Contains(err.Error(), "video.mode") {
			t.Fatalf("removed size accepted: %s, %v", wire, err)
		}
	}
	cfg.Targets[0].Video.Mode = "1920x1080@60:invalid"
	if err := validateConfig(cfg); err == nil {
		t.Fatal("invalid mode accepted")
	}
}

func TestWebVideoModeDiscovery(t *testing.T) {
	previous := listWebVideoModes
	defer func() { listWebVideoModes = previous }()
	listWebVideoModes = func(device string) ([]capture.VideoMode, error) {
		if device != "Hagibis" {
			t.Fatalf("unexpected device %q", device)
		}
		return []capture.VideoMode{{Width: 1920, Height: 1080, PixelFormat: "nv12", MinFPS: 30, MaxFPS: 60}}, nil
	}
	handler := newWebConfigHandler("unused", DefaultConfig())
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, httptest.NewRequest(http.MethodGet, "/api/video-modes?device=Hagibis", nil))
	var body struct {
		Modes []capture.VideoMode `json:"modes"`
	}
	if result.Code != 200 || json.Unmarshal(result.Body.Bytes(), &body) != nil || len(body.Modes) != 1 || body.Modes[0].PixelFormat != "nv12" {
		t.Fatalf("invalid discovery: %d %s", result.Code, result.Body.String())
	}
	for _, tc := range []struct {
		method, url string
		status      int
	}{{"POST", "/api/video-modes?device=Hagibis", 405}, {"GET", "/api/video-modes", 400}} {
		result = httptest.NewRecorder()
		handler.ServeHTTP(result, httptest.NewRequest(tc.method, tc.url, nil))
		if result.Code != tc.status {
			t.Fatalf("%s %s: %d", tc.method, tc.url, result.Code)
		}
	}
}
