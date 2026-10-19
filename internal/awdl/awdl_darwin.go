//go:build darwin

package awdl

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

type systemInterface struct{}

func newSystemInterface() networkInterface { return systemInterface{} }

func (systemInterface) IsUp() (bool, error) {
	iface, err := net.InterfaceByName("awdl0")
	if err != nil {
		return false, err
	}
	return iface.Flags&net.FlagUp != 0, nil
}

func (systemInterface) SetUp(up bool) error {
	state := "down"
	if up {
		state = "up"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	name, args := "/sbin/ifconfig", []string{"awdl0", state}
	if os.Geteuid() != 0 {
		// Never prompt for a password on a protocol callback or background worker.
		name, args = "/usr/bin/sudo", []string{"-n", name, "awdl0", state}
	}
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
