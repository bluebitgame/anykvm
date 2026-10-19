package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"anykvm/internal/discovery"
)

func TestWebConfigYAMLReturnsFileVerbatim(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Web.HTTP = ":8080"
	cfg.Web.Password = "viewer-secret"
	cfg.AdminPassword = "admin-secret"
	cfg.VNC.Addr = ":5900"
	cfg.VNC.Password = "vnc-secret"
	cfg.RDP.Addr = ":3389"
	cfg.RDP.Password = "rdp-secret"
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := SaveWebConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	original := "# Keep this comment and the original formatting.\n" + string(data)
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	handler := newWebConfigHandler(path, cfg)

	get := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	getResult := httptest.NewRecorder()
	handler.ServeHTTP(getResult, get)
	if getResult.Code != http.StatusOK {
		t.Fatalf("GET status = %d: %s", getResult.Code, getResult.Body.String())
	}
	var exported struct {
		YAML string `json:"yaml"`
	}
	if err := json.Unmarshal(getResult.Body.Bytes(), &exported); err != nil {
		t.Fatal(err)
	}
	if exported.YAML != original {
		t.Fatal("YAML endpoint did not return the original file contents")
	}

	body, err := json.Marshal(exported)
	if err != nil {
		t.Fatal(err)
	}
	put := httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(body))
	put.Header.Set("Content-Type", "application/json")
	putResult := httptest.NewRecorder()
	handler.ServeHTTP(putResult, put)
	if putResult.Code != http.StatusOK {
		t.Fatalf("PUT status = %d: %s", putResult.Code, putResult.Body.String())
	}
	saved, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Web.Password != cfg.Web.Password || saved.AdminPassword != cfg.AdminPassword || saved.VNC.Password != cfg.VNC.Password || saved.RDP.Password != cfg.RDP.Password {
		t.Fatalf("secrets changed after settings save: %+v", saved)
	}
}

func TestWebConfigJSONRedactsAndRetainsSecrets(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Web.HTTP = ":8080"
	cfg.Web.Password = "viewer-secret"
	cfg.AdminPassword = "admin-secret"
	cfg.VNC.Addr = ":5900"
	cfg.VNC.Password = "vnc-secret"
	cfg.RDP.Addr = ":3389"
	cfg.RDP.Password = "rdp-secret"
	path := filepath.Join(t.TempDir(), "config.yaml")
	handler := newWebConfigHandler(path, cfg)

	get := httptest.NewRequest(http.MethodGet, "/api/config.json", nil)
	getResult := httptest.NewRecorder()
	handler.ServeHTTP(getResult, get)
	if getResult.Code != http.StatusOK {
		t.Fatalf("GET status = %d: %s", getResult.Code, getResult.Body.String())
	}
	if strings.Contains(getResult.Body.String(), "viewer-secret") || strings.Contains(getResult.Body.String(), "admin-secret") || strings.Contains(getResult.Body.String(), "vnc-secret") || strings.Contains(getResult.Body.String(), "rdp-secret") {
		t.Fatal("JSON settings endpoint disclosed a secret")
	}
	var exported Config
	if err := json.Unmarshal(getResult.Body.Bytes(), &exported); err != nil {
		t.Fatal(err)
	}
	if exported.Web.Password != retainedSecret || exported.AdminPassword != retainedSecret || exported.VNC.Password != retainedSecret || exported.RDP.Password != retainedSecret {
		t.Fatalf("JSON secrets were not redacted: %+v", exported)
	}
	exported.Web.HTTP = ":8080"
	body, err := json.Marshal(exported)
	if err != nil {
		t.Fatal(err)
	}
	put := httptest.NewRequest(http.MethodPut, "/api/config.json", bytes.NewReader(body))
	put.Header.Set("Content-Type", "application/json")
	putResult := httptest.NewRecorder()
	handler.ServeHTTP(putResult, put)
	if putResult.Code != http.StatusOK {
		t.Fatalf("PUT status = %d: %s", putResult.Code, putResult.Body.String())
	}
	saved, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Web.Password != cfg.Web.Password || saved.AdminPassword != cfg.AdminPassword || saved.VNC.Password != cfg.VNC.Password || saved.RDP.Password != cfg.RDP.Password {
		t.Fatalf("secrets changed after JSON settings save: %+v", saved)
	}
	if saved.Web.HTTP != ":8080" {
		t.Fatalf("HTTP address = %q, want :8080", saved.Web.HTTP)
	}
}

func TestSaveWebConfigOmitsDisabledServicesAndDefaults(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Web.HTTP = ":8080"
	cfg.Targets = []TargetConfig{{
		Name:  "Office Mac",
		Video: VideoConfig{Source: "screen:0"},
		HID:   HIDConfig{Device: "local", MouseMode: "absolute"},
	}}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := SaveWebConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, omitted := range []string{"vnc:", "rdp:", "gamestream:", "buffer_frames:", "hotkey_mod:", "mouse_mode:"} {
		if strings.Contains(text, omitted) {
			t.Errorf("minimal web config unexpectedly contains %q:\n%s", omitted, text)
		}
	}
	for _, required := range []string{"web:", "http: :8080", "targets:", "name: Office Mac", "source: screen:0"} {
		if !strings.Contains(text, required) {
			t.Errorf("minimal web config is missing %q:\n%s", required, text)
		}
	}
	saved, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Web.HTTP != cfg.Web.HTTP || saved.Web.BufferFrames != DefaultConfig().Web.BufferFrames {
		t.Fatalf("Web defaults were not restored: %+v", saved.Web)
	}
	if len(saved.Targets) != 1 || saved.Targets[0].Name != "Office Mac" || saved.Targets[0].Video.Source != "screen:0" {
		t.Fatalf("target did not round-trip: %+v", saved.Targets)
	}
}

func TestWebConfigRejectsInvalidConfiguration(t *testing.T) {
	cfg := DefaultConfig()
	path := filepath.Join(t.TempDir(), "config.yaml")
	handler := newWebConfigHandler(path, cfg)
	body, err := json.Marshal(map[string]string{"yaml": "web: {buffer_frames: 9}"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "buffer_frames") {
		t.Fatalf("invalid settings response = %d: %s", response.Code, response.Body.String())
	}
}

func TestWebConfigRDPPoliciesRoundTrip(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Web.HTTP = "127.0.0.1:8080"
	cfg.RDP.Addr = ":3389"
	cfg.RDP.Password = "test-secret"
	path := filepath.Join(t.TempDir(), "config.yaml")
	handler := newWebConfigHandler(path, cfg)
	candidate := cfg
	candidate.RDP.Codecs.H264 = false
	candidate.RDP.ClipboardOptions.Text = 1
	candidate.RDP.ClipboardOptions.RichText = 2
	candidate.RDP.ClipboardOptions.Images = 0
	candidate.RDP.Drive.AutoMount = true
	candidate.RDP.Drive.Permissions = 1
	body, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/config.json?apply=1", bytes.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("save failed: %s", response.Body.String())
	}
	saved, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.RDP.Codecs != candidate.RDP.Codecs || saved.RDP.ClipboardOptions != candidate.RDP.ClipboardOptions || saved.RDP.Drive != candidate.RDP.Drive {
		t.Fatal("RDP policies changed after saving")
	}
	var result configSaveResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.RestartReasons, []string{"RDP clipboard settings", "RDP WebDAV settings"}) {
		t.Fatalf("restart reasons=%v", result.RestartReasons)
	}
}

func TestWebConfigReportsAppliedAndRestartChanges(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Web.HTTP = ":8080"
	path := filepath.Join(t.TempDir(), "config.yaml")
	called := false
	handler := newWebConfigHandler(path, cfg, func(previous, next Config) configSaveResult {
		called = true
		if previous.Web.Password != "" || next.Web.Password != "viewer-password" {
			t.Fatalf("unexpected credentials: previous=%q next=%q", previous.Web.Password, next.Web.Password)
		}
		return configSaveResult{
			Applied:         []string{"Web authentication"},
			RestartRequired: true,
			RestartReasons:  []string{"Target configuration"},
		}
	})
	candidate := cfg
	candidate.Web.Password = "viewer-password"
	candidate.Targets[0].Name = "Other target"
	body, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, "/api/config.json?apply=1", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("save status = %d: %s", response.Code, response.Body.String())
	}
	if !called {
		t.Fatal("runtime applier was not called")
	}
	var result configSaveResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.RestartRequired || !reflect.DeepEqual(result.Applied, []string{"Web authentication"}) || !reflect.DeepEqual(result.RestartReasons, []string{"Target configuration"}) {
		t.Fatalf("save result = %+v", result)
	}
}

func TestSettingsPageServed(t *testing.T) {
	handler := newWebConfigHandler(filepath.Join(t.TempDir(), "config.yaml"), DefaultConfig())
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("settings page status = %d", response.Code)
	}
}

func TestWebConfigListsConfigDevices(t *testing.T) {
	previous := listWebConfigDevices
	listWebConfigDevices = func() map[string][]discovery.DeviceOption {
		return map[string][]discovery.DeviceOption{
			"video": {{Value: "screen:0", Label: "Local screen"}},
			"audio": {{Value: "Mic", Label: "Mic"}},
			"hid":   {{Value: "local", Label: "Local input"}},
		}
	}
	t.Cleanup(func() { listWebConfigDevices = previous })

	cfg := DefaultConfig()
	cfg.Targets[0].HID.Device = "ch9329:COM5"
	handler := newWebConfigHandler(filepath.Join(t.TempDir(), "config.yaml"), cfg)
	request := httptest.NewRequest(http.MethodGet, "/api/devices", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("device listing status = %d: %s", response.Code, response.Body.String())
	}
	var got map[string][]discovery.DeviceOption
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got["video"], []discovery.DeviceOption{{Value: "screen:0", Label: "Local screen"}}) || !reflect.DeepEqual(got["audio"], []discovery.DeviceOption{{Value: "Mic", Label: "Mic"}}) || !reflect.DeepEqual(got["hid"], []discovery.DeviceOption{{Value: "local", Label: "Local input"}}) {
		t.Fatalf("device listing = %#v", got)
	}
}
