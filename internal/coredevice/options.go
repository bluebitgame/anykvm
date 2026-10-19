// Package coredevice connects directly to iPhone developer services using Go.
package coredevice

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Options struct {
	RSD     string        `yaml:"rsd,omitempty"`     // Optional reachable HOST:PORT of an existing kernel tunnel.
	Timeout time.Duration `yaml:"timeout,omitempty"` // Startup deadline, default 60s.
}

// Reject removed runtime options instead of silently accepting a stale setup.
func (o *Options) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("coredevice must be a mapping")
	}
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if key != "rsd" && key != "timeout" {
			return fmt.Errorf("unknown coredevice option %q", key)
		}
	}
	type plainOptions Options
	return node.Decode((*plainOptions)(o))
}

func IsDevice(s string) bool { return s == "coredevice" || strings.HasPrefix(s, "coredevice:") }

func DeviceUDID(s string) (string, error) {
	if s == "coredevice" {
		return "", nil
	}
	if !strings.HasPrefix(s, "coredevice:") {
		return "", fmt.Errorf("coredevice: expected coredevice[:UDID]")
	}
	id := strings.TrimPrefix(s, "coredevice:")
	if id == "" || len(id) > 128 || strings.IndexFunc(id, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-')
	}) >= 0 {
		return "", fmt.Errorf("coredevice: invalid device UDID")
	}
	return id, nil
}

func (o Options) Validate() error {
	if o.Timeout < 0 {
		return fmt.Errorf("coredevice.timeout must be positive")
	}
	if o.RSD != "" {
		host, port, err := net.SplitHostPort(o.RSD)
		if err != nil || host == "" {
			return fmt.Errorf("coredevice.rsd must be HOST:PORT (bracket IPv6 addresses)")
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("coredevice.rsd port must be 1..65535")
		}
	}
	return nil
}
