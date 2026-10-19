package gamestream

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
)

// ListenerAddresses derives the HTTPS pairing address from a validated base port.
func ListenerAddresses(addr string) (httpAddr, httpsAddr string, err error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return "", "", fmt.Errorf("gamestream address: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1029 || port > 65514 {
		return "", "", errors.New("gamestream base port must be 1029..65514")
	}
	return addr, net.JoinHostPort(host, strconv.Itoa(port-5)), nil
}

// ResolveStatePath uses state.json beside the executable when no path is supplied.
// Explicit relative paths are preserved and resolve from the working directory.
func ResolveStatePath(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable for GameStream state: %w", err)
	}
	return filepath.Join(filepath.Dir(executable), "state.json"), nil
}

// ValidateH264Profile checks the native VideoToolbox profile setting.
func ValidateH264Profile(profile string) error {
	switch profile {
	case "", "constrained_baseline", "baseline", "main", "high":
		return nil
	default:
		return fmt.Errorf("gamestream.profile must be constrained_baseline, baseline, main or high")
	}
}
