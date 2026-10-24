package spice

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
)

// endpoint keeps local socket paths distinct from TCP authorities.
type endpoint struct {
	path, host, port string
}

func (e endpoint) remote() bool { return e.host != "" }
func (e endpoint) address() string {
	if e.remote() {
		return net.JoinHostPort(e.host, e.port)
	}
	return e.path
}

func parseEndpoint(raw string) (endpoint, error) {
	invalid := func() (endpoint, error) {
		return endpoint{}, fmt.Errorf("invalid SPICE source; use spice:/absolute/path.sock or spice://host:port")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "spice" || u.User != nil || u.RawQuery != "" ||
		u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return invalid()
	}
	_, address, _ := strings.Cut(raw, ":")
	if strings.HasPrefix(address, "//") {
		if u.Host == "" || u.Path != "" {
			return invalid()
		}
		host, port, err := net.SplitHostPort(u.Host)
		number, portErr := strconv.Atoi(port)
		if err != nil || portErr != nil || number < 1 || number > 65535 || host == "" ||
			strings.ContainsAny(host, "\x00\r\n /\\") {
			return invalid()
		}
		return endpoint{host: host, port: strconv.Itoa(number)}, nil
	}
	if u.Host != "" || !filepath.IsAbs(u.Path) || strings.ContainsAny(u.Path, "\x00\r\n") || len(u.Path) > 107 {
		return invalid()
	}
	return endpoint{path: u.Path}, nil
}

// socketPath is used by local-only callers and tests.
func socketPath(raw string) (string, error) {
	e, err := parseEndpoint(raw)
	if err != nil {
		return "", err
	}
	if e.remote() {
		return "", fmt.Errorf("SPICE DMA-BUF capture requires a local Unix socket")
	}
	return e.path, nil
}
