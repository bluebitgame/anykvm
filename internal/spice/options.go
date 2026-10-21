// Package spice provides a Linux local SPICE GL upstream. Raw pixel fallback is
// deliberately unavailable: successful video output proves DMA-BUF GPU encoding.
package spice

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"anykvm/internal/capture"
)

type Options struct {
	Password    string        `yaml:"password,omitempty"`
	PasswordEnv string        `yaml:"password_env,omitempty"`
	RenderNode  string        `yaml:"render_node,omitempty"`
	Timeout     time.Duration `yaml:"timeout,omitempty"`
}

func IsDevice(raw string) bool {
	return strings.HasPrefix(strings.ToLower(raw), "spice:")
}

func socketPath(raw string) (string, error) {
	u, err := url.Parse(raw)
	_, address, _ := strings.Cut(raw, ":")
	// SPICE currently accepts a local protocol:path only. Reserve // authority
	// syntax for a future remote transport rather than accepting ambiguous URIs.
	if err != nil || u.Scheme != "spice" || strings.HasPrefix(address, "//") ||
		u.Host != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || u.Opaque != "" || !filepath.IsAbs(u.Path) ||
		strings.ContainsAny(u.Path, "\x00\r\n") || len(u.Path) > 107 {
		return "", fmt.Errorf("SPICE GPU upstream requires a local Unix socket: spice:/absolute/path.sock (TCP cannot carry DMA-BUF)")
	}
	return u.Path, nil
}

func ValidateSource(raw string, o Options) error {
	if !IsDevice(raw) {
		return nil
	}
	if _, err := socketPath(raw); err != nil {
		return err
	}
	if o.Password != "" && o.PasswordEnv != "" {
		return fmt.Errorf("use only one of spice.password and password_env")
	}
	if strings.ContainsAny(o.Password+o.PasswordEnv+o.RenderNode, "\x00\r\n") {
		return fmt.Errorf("invalid SPICE option")
	}
	if o.RenderNode != "" && (!strings.HasPrefix(o.RenderNode, "/dev/dri/renderD") || strings.Contains(o.RenderNode, "..") || filepath.Clean(o.RenderNode) != o.RenderNode) {
		return fmt.Errorf("spice.render_node must name /dev/dri/renderD*")
	}
	if o.Timeout < 0 || o.Timeout > 5*time.Minute {
		return fmt.Errorf("spice.timeout must be 0..5m (0 uses 15s)")
	}
	return nil
}

func NewCapture(raw string, o Options) (capture.Device, bool) {
	if !IsDevice(raw) {
		return nil, false
	}
	if err := ValidateSource(raw, o); err != nil {
		return capture.Failed(err), true
	}
	return newDevice(raw, o), true
}
