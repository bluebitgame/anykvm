package app

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"

	"anykvm/internal/capture"
	"anykvm/internal/discovery"
	"anykvm/internal/wsmedia"

	"gopkg.in/yaml.v3"
)

// retainedSecret keeps a stored credential when the settings page saves
// other fields without revealing or replacing it.
const retainedSecret = "__ANYKVM_KEEP_EXISTING_SECRET__"

// configSaveResult reports which settings were applied to the running server
// and which were only persisted for the next launch.
type configSaveResult struct {
	Applied                       []string `json:"applied,omitempty"`
	RestartRequired               bool     `json:"restartRequired"`
	RestartReasons                []string `json:"restartReasons,omitempty"`
	SettingsAuthenticationChanged bool     `json:"settingsAuthenticationChanged,omitempty"`
}

type configApplier func(previous, next Config) configSaveResult

type webConfigHandler struct {
	path string

	mu      sync.RWMutex
	cfg     Config // last saved configuration
	runtime Config // configuration currently applied to this process
	apply   configApplier
}

var listWebConfigDevices = discovery.ConfigDevices
var listWebVideoModes = capture.ListVideoModes

func newWebConfigHandler(path string, cfg Config, appliers ...configApplier) http.Handler {
	var apply configApplier
	if len(appliers) != 0 {
		apply = appliers[0]
	}
	return &webConfigHandler{path: path, cfg: cfg, runtime: cfg, apply: apply}
}

func (h *webConfigHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "", "/":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(wsmedia.SettingsPageForPlatform(runtime.GOOS)))
	case "/api/config":
		h.handleConfig(w, r)
	case "/api/config.json":
		h.handleJSONConfig(w, r)
	case "/api/video-modes":
		h.handleVideoModes(w, r)
	case "/api/devices":
		h.handleDevices(w, r)
	default:
		http.NotFound(w, r)
	}
}

// handleVideoModes queries metadata only; it never opens a capture session.
func (h *webConfigHandler) handleVideoModes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	device := strings.TrimSpace(r.URL.Query().Get("device"))
	if device == "" {
		http.Error(w, "device is required", http.StatusBadRequest)
		return
	}
	modes, err := listWebVideoModes(device)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if modes == nil {
		modes = []capture.VideoMode{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Modes []capture.VideoMode `json:"modes"`
	}{modes})
}

// handleDevices provides the settings form with configuration-ready choices
// discovered on this host. The form keeps the values editable for sources that
// cannot be discovered locally.
func (h *webConfigHandler) handleDevices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(listWebConfigDevices())
}

// handleJSONConfig provides the form editor with the complete typed model.
// The view redacts credentials and the same save path validates and writes it
// as the YAML editor, so fields not represented by the form survive intact.
func (h *webConfigHandler) handleJSONConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		h.mu.RLock()
		cfg := webConfigView(h.cfg)
		h.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cfg)
	case http.MethodPut:
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		candidate := DefaultConfig()
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&candidate); err != nil || decoder.Decode(new(any)) != io.EOF {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			http.Error(w, "invalid configuration: "+err.Error(), http.StatusBadRequest)
			return
		}
		h.saveWebConfig(w, candidate, r.URL.Query().Get("apply") == "1")
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPut)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *webConfigHandler) saveWebConfig(w http.ResponseWriter, candidate Config, apply bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	candidate.restoreRetainedSecrets(h.cfg)
	if err := validateConfig(candidate); err != nil {
		http.Error(w, "invalid configuration: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := SaveWebConfig(h.path, candidate); err != nil {
		http.Error(w, "save configuration: "+err.Error(), http.StatusInternalServerError)
		return
	}
	result := configSaveResult{}
	if apply {
		result = configSaveResult{RestartReasons: restartReasons(h.runtime, candidate)}
		result.RestartRequired = len(result.RestartReasons) != 0
		if h.apply != nil {
			result = h.apply(h.runtime, candidate)
		}
		h.runtime = candidate
	}
	h.cfg = candidate
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// restartReasons describes changes whose runtime owns listeners, capture
// devices, or protocol state and therefore cannot be safely rebuilt in place.
func restartReasons(previous, next Config) []string {
	var reasons []string
	if previous.Viewer != next.Viewer {
		reasons = append(reasons, "Local viewer mode")
	}
	if previous.Web.HTTP != next.Web.HTTP || previous.Web.HTTPS != next.Web.HTTPS {
		reasons = append(reasons, "Web listener addresses")
	}
	previousVNC := previous.VNC
	previousVNC.Codecs = next.VNC.Codecs
	previousVNC.Bitrate = next.VNC.Bitrate
	previousVNC.PixelTol, previousVNC.JPEGQuality, previousVNC.ForceCont = next.VNC.PixelTol, next.VNC.JPEGQuality, next.VNC.ForceCont
	if !reflect.DeepEqual(previousVNC, next.VNC) {
		reasons = append(reasons, "VNC service settings")
	}
	previousRDP := previous.RDP
	if previous.RDP.Clipboard != next.RDP.Clipboard || previous.RDP.ClipboardOptions != next.RDP.ClipboardOptions {
		reasons = append(reasons, "RDP clipboard settings")
	}
	if previous.RDP.Drive != next.RDP.Drive {
		reasons = append(reasons, "RDP WebDAV settings")
	}
	previousRDP.Clipboard, previousRDP.ClipboardOptions, previousRDP.Drive = next.RDP.Clipboard, next.RDP.ClipboardOptions, next.RDP.Drive
	previousRDP.Username, previousRDP.Password = next.RDP.Username, next.RDP.Password
	previousRDP.Codecs, previousRDP.Bitrate, previousRDP.BufferFrames = next.RDP.Codecs, next.RDP.Bitrate, next.RDP.BufferFrames
	previousRDP.Codecs.HEVC = previous.RDP.Codecs.HEVC
	previousRDP.ProgressiveQuantization, previousRDP.ProgressiveRefinement = next.RDP.ProgressiveQuantization, next.RDP.ProgressiveRefinement
	if !reflect.DeepEqual(previousRDP, next.RDP) {
		reasons = append(reasons, "RDP service settings")
	}
	if !reflect.DeepEqual(previous.GameStream, next.GameStream) {
		reasons = append(reasons, "GameStream service settings")
	}
	if previous.TLSCert != next.TLSCert || previous.TLSKey != next.TLSKey {
		reasons = append(reasons, "TLS certificate settings")
	}
	if previous.DisableAWDL != next.DisableAWDL {
		reasons = append(reasons, "AWDL setting")
	}
	if previous.HotkeyMod != next.HotkeyMod {
		reasons = append(reasons, "Target-switch modifier")
	}
	if !reflect.DeepEqual(previous.Targets, next.Targets) {
		reasons = append(reasons, "Target configuration")
	}
	return reasons
}

func withoutRestartReason(reasons []string, remove string) []string {
	out := reasons[:0]
	for _, reason := range reasons {
		if reason != remove {
			out = append(out, reason)
		}
	}
	return out
}

func (h *webConfigHandler) handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		data, err := os.ReadFile(h.path)
		if err != nil {
			http.Error(w, "read configuration: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			YAML string `json:"yaml"`
		}{YAML: string(data)})
	case http.MethodPut:
		var request struct {
			YAML string `json:"yaml"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(request.YAML) == "" {
			http.Error(w, "configuration cannot be empty", http.StatusBadRequest)
			return
		}
		candidate := DefaultConfig()
		if err := yaml.Unmarshal([]byte(request.YAML), &candidate); err != nil {
			http.Error(w, "invalid YAML: "+err.Error(), http.StatusBadRequest)
			return
		}

		h.saveWebConfig(w, candidate, r.URL.Query().Get("apply") == "1")
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPut)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func webConfigView(cfg Config) Config {
	cfg.Targets = append([]TargetConfig(nil), cfg.Targets...)
	cfg.Web.Password = redactedSecret(cfg.Web.Password)
	cfg.AdminPassword = redactedSecret(cfg.AdminPassword)
	cfg.VNC.Password = redactedSecret(cfg.VNC.Password)
	cfg.RDP.Password = redactedSecret(cfg.RDP.Password)
	for i := range cfg.Targets {
		cfg.Targets[i].ARD.Password = redactedSecret(cfg.Targets[i].ARD.Password)
		cfg.Targets[i].RTSP.Password = redactedSecret(cfg.Targets[i].RTSP.Password)
		cfg.Targets[i].SPICE.Password = redactedSecret(cfg.Targets[i].SPICE.Password)
		cfg.Targets[i].RDP.Password = redactedSecret(cfg.Targets[i].RDP.Password)
		cfg.Targets[i].Video.Source = redactSourceURL(cfg.Targets[i].Video.Source)
	}
	return cfg
}

func redactedSecret(secret string) string {
	if secret == "" {
		return ""
	}
	return retainedSecret
}

func (cfg *Config) restoreRetainedSecrets(previous Config) {
	cfg.Web.Password = restoreSecret(cfg.Web.Password, previous.Web.Password)
	cfg.AdminPassword = restoreSecret(cfg.AdminPassword, previous.AdminPassword)
	cfg.VNC.Password = restoreSecret(cfg.VNC.Password, previous.VNC.Password)
	cfg.RDP.Password = restoreSecret(cfg.RDP.Password, previous.RDP.Password)
	previousARDPasswords := make(map[string]string, len(previous.Targets))
	previousRTSPPasswords := make(map[string]string, len(previous.Targets))
	previousSPICEPasswords := make(map[string]string, len(previous.Targets))
	previousRDPPasswords := make(map[string]string, len(previous.Targets))
	previousSources := make(map[string]string, len(previous.Targets))
	for _, target := range previous.Targets {
		if target.Name != "" {
			previousARDPasswords[target.Name] = target.ARD.Password
			previousRTSPPasswords[target.Name] = target.RTSP.Password
			previousSPICEPasswords[target.Name] = target.SPICE.Password
			previousRDPPasswords[target.Name] = target.RDP.Password
			previousSources[target.Name] = target.Video.Source
		}
	}
	for i := range cfg.Targets {
		name := cfg.Targets[i].Name
		cfg.Targets[i].Video.Source = restoreSourceURL(cfg.Targets[i].Video.Source, previousSources[name])
		cfg.Targets[i].RTSP.Password = restoreSecret(cfg.Targets[i].RTSP.Password, previousRTSPPasswords[name])
		cfg.Targets[i].SPICE.Password = restoreSecret(cfg.Targets[i].SPICE.Password, previousSPICEPasswords[name])
		cfg.Targets[i].RDP.Password = restoreSecret(cfg.Targets[i].RDP.Password, previousRDPPasswords[name])
		if previous, ok := previousARDPasswords[cfg.Targets[i].Name]; ok {
			cfg.Targets[i].ARD.Password = restoreSecret(cfg.Targets[i].ARD.Password, previous)
		}
	}
}

func restoreSecret(updated, previous string) string {
	if updated == retainedSecret {
		return previous
	}
	return updated
}
