package gamestream

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

// ClientOS describes the client OS or OS family, independently of the target.
type ClientOS string

const (
	ClientOSUnknown ClientOS = "unknown"
	ClientOSDarwin  ClientOS = "darwin" // Apple system family; exact macOS/iOS is unknown
	ClientOSMacOS   ClientOS = "macos"
	ClientOSWindows ClientOS = "windows"
	ClientOSLinux   ClientOS = "linux"
	ClientOSAndroid ClientOS = "android"
	ClientOSIOS     ClientOS = "ios"
)

// ClientInfo is a session snapshot for platform-specific input policies.
// OS/family is metadata for input policies, never authentication evidence.
type ClientInfo struct {
	OS                     ClientOS
	OSSource               string // config, user-agent, or unknown
	CertificateFingerprint string // lowercase SHA-256 of the paired certificate DER
	UserAgent              string
}

// UsesAppleShortcuts selects the shared macOS/iOS shortcut convention. It does
// not install any mapping; the mapping direction also depends on the target OS.
func (c ClientInfo) UsesAppleShortcuts() bool {
	return c.OS == ClientOSDarwin || c.OS == ClientOSMacOS || c.OS == ClientOSIOS
}

func userAgentIsDarwin(agent string) bool {
	for _, product := range strings.Fields(strings.ToLower(agent)) {
		if strings.HasPrefix(product, "darwin/") && len(product) > len("darwin/") {
			return true
		}
	}
	return false
}

func parseClientOS(value string) (ClientOS, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "unknown":
		return ClientOSUnknown, true
	case "darwin":
		return ClientOSDarwin, true
	case "macos", "mac", "osx", "mac os x":
		return ClientOSMacOS, true
	case "windows", "win32", "win64":
		return ClientOSWindows, true
	case "linux":
		return ClientOSLinux, true
	case "android":
		return ClientOSAndroid, true
	case "ios", "ipados", "tvos":
		return ClientOSIOS, true
	}
	return ClientOSUnknown, false
}

// ParseClientOSOverrides validates and copies per-certificate configuration.
// Certificate fingerprints avoid ambiguous device names, IPs and uniqueid values.
func ParseClientOSOverrides(values map[string]string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		fp := strings.ToLower(strings.TrimSpace(key))
		data, err := hex.DecodeString(fp)
		if err != nil || len(data) != 32 {
			return nil, fmt.Errorf("gamestream.client_os: %q must be a SHA-256 certificate fingerprint", key)
		}
		os, ok := parseClientOS(value)
		if !ok {
			return nil, fmt.Errorf("gamestream.client_os[%q]: unsupported OS %q", key, value)
		}
		if _, exists := out[fp]; exists {
			return nil, fmt.Errorf("gamestream.client_os: duplicate certificate fingerprint %q", key)
		}
		out[fp] = string(os)
	}
	return out, nil
}

// sessionClientInfo resolves only a currently paired TLS certificate. Official
// clients provide no reliable exact OS field. A Darwin UA product identifies
// only the shared Apple family; its absence provides no OS evidence.
func (s *Server) sessionClientInfo(r *http.Request) ClientInfo {
	info := ClientInfo{OS: ClientOSUnknown, OSSource: "unknown", UserAgent: r.UserAgent()}
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ClientInfo{OS: ClientOSUnknown, OSSource: "unknown"}
	}
	fp := fingerprint(r.TLS.PeerCertificates[0])
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, paired := s.state.Clients[fp]; !paired {
		return ClientInfo{OS: ClientOSUnknown, OSSource: "unknown"}
	}
	info.CertificateFingerprint = fp
	if os, ok := s.cfg.ClientOS[fp]; ok {
		info.OS, info.OSSource = ClientOS(os), "config"
	} else if userAgentIsDarwin(info.UserAgent) {
		info.OS, info.OSSource = ClientOSDarwin, "user-agent"
	}
	return info
}
