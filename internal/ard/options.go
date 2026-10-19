// Package ard implements an experimental Apple Screen Sharing capture source.
package ard

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Options keeps upstream credentials separate from downstream server accounts.
// PasswordEnv is resolved on every Open, so configuration need not contain a secret.
type Options struct {
	Username    string        `yaml:"username,omitempty"`
	Password    string        `yaml:"password,omitempty"`
	PasswordEnv string        `yaml:"password_env,omitempty"`
	Codec       string        `yaml:"codec,omitempty"`        // auto, hevc, h264 (not yet verified)
	DisplayMode string        `yaml:"display_mode,omitempty"` // mirror; private is not implemented
	Timeout     time.Duration `yaml:"timeout,omitempty"`
	Fallback    string        `yaml:"fallback,omitempty"` // deny (default), vnc
}

func IsDevice(id string) bool { return strings.HasPrefix(strings.ToLower(id), "ard://") }

func ParseAddress(id string) (string, error) {
	u, err := url.Parse(id)
	if err != nil || u == nil || strings.ToLower(u.Scheme) != "ard" || u.Hostname() == "" {
		return "", fmt.Errorf("ard: expected ard://host[:port]")
	}
	// Do not include the supplied URI in errors: even malformed URIs can contain secrets.
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "" {
		return "", fmt.Errorf("ard: device URI must contain only a host and port; use ard options for credentials")
	}
	p := u.Port()
	if p == "" {
		p = "5900"
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("ard: invalid port")
	}
	return net.JoinHostPort(u.Hostname(), p), nil
}

func (o Options) normalized() Options {
	if o.Codec == "" {
		o.Codec = "auto"
	}
	if o.DisplayMode == "" {
		o.DisplayMode = "mirror"
	}
	if o.Fallback == "" {
		o.Fallback = "deny"
	}
	if o.Timeout == 0 {
		o.Timeout = 10 * time.Second
	}
	return o
}

func (o Options) Validate() error {
	o = o.normalized()
	if o.Codec != "auto" && o.Codec != "h264" && o.Codec != "hevc" {
		return fmt.Errorf("ard.codec: expected auto, h264 or hevc")
	}
	if o.DisplayMode != "mirror" && o.DisplayMode != "private" {
		return fmt.Errorf("ard.display_mode: expected mirror or private")
	}
	if o.Fallback != "deny" && o.Fallback != "vnc" {
		return fmt.Errorf("ard.fallback: expected deny or vnc")
	}
	if o.Timeout < time.Millisecond || o.Timeout > 5*time.Minute {
		return fmt.Errorf("ard.timeout: expected 1ms through 5m")
	}
	if o.Password != "" && o.PasswordEnv != "" {
		return fmt.Errorf("ard: password and password_env are mutually exclusive")
	}
	if len(o.Username) > 63 || len(o.Password) > 63 || strings.ContainsRune(o.Username+o.Password, 0) {
		return fmt.Errorf("ard: account credentials must be at most 63 UTF-8 bytes and contain no NUL")
	}
	return nil
}

func (o Options) credentials() (string, error) {
	p := o.Password
	if o.PasswordEnv != "" {
		var ok bool
		p, ok = os.LookupEnv(o.PasswordEnv)
		if !ok || p == "" {
			return "", fmt.Errorf("ard: credential environment variable is missing or empty")
		}
	}
	if o.Username == "" || p == "" {
		return "", fmt.Errorf("ard: username and password or password_env are required")
	}
	if len(p) > 63 || strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("ard: password must be at most 63 UTF-8 bytes and contain no NUL")
	}
	return p, nil
}
