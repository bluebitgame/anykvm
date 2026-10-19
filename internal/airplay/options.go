// Package airplay implements the native AirPlay receiver protocol.
// The receiver uses legacy pairing and H.264 mirroring. Compatibility with
// physical iPhones and recent iOS releases requires device validation.
package airplay

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

type Options struct {
	Addr      string `yaml:"addr,omitempty"`      // Native receiver control listener; default :7000.
	Interface string `yaml:"interface,omitempty"` // Optional interface for mDNS advertisement.
}

func IsDevice(s string) bool { return s == "airplay" || strings.HasPrefix(s, "airplay:") }

// ReceiverName is the name to advertise on the iPhone's Screen Mirroring menu,
// not a filter identifying a remote iPhone.
func ReceiverName(device string) (string, error) {
	if !IsDevice(device) {
		return "", fmt.Errorf("airplay: expected airplay[:receiver-name]")
	}
	name := "AnyKVM"
	if device != "airplay" {
		name = strings.TrimPrefix(device, "airplay:")
	}
	if strings.TrimSpace(name) == "" || len(name) > 63 || !utf8.ValidString(name) || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("airplay: receiver name must contain 1..63 UTF-8 bytes without control characters")
	}
	return name, nil
}

func (o Options) address() string {
	if o.Addr == "" {
		return ":7000"
	}
	return o.Addr
}

func (o Options) Validate() error {
	host, port, err := net.SplitHostPort(o.address())
	if err != nil {
		return fmt.Errorf("airplay.addr must be IP:PORT or :PORT")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("airplay.addr port must be 1..65535")
	}
	if host != "" && net.ParseIP(host) == nil {
		return fmt.Errorf("airplay.addr host must be an IP address")
	}
	return nil
}

func (o *Options) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("airplay must be a mapping")
	}
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if key != "addr" && key != "interface" {
			return fmt.Errorf("unknown airplay option %q", key)
		}
	}
	type plain Options
	return node.Decode((*plain)(o))
}
