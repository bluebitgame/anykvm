//go:build darwin || linux

package coredevice

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strconv"

	"github.com/danielpaulus/go-ios/ios"
	"github.com/songgao/water"
)

func openUSBTunnel(ctx context.Context, device ios.DeviceEntry) (string, int, io.Closer, error) {
	usb, err := connectUSBService(ctx, device, "com.apple.internal.devicecompute.CoreDeviceProxy")
	if err != nil {
		return "", 0, nil, err
	}
	success := false
	defer func() {
		if !success {
			usb.Close()
		}
	}()
	p, err := handshakeTunnel(usb)
	if err != nil {
		return "", 0, nil, err
	}
	tun, err := water.New(water.Config{DeviceType: water.TUN})
	if err != nil {
		return "", 0, nil, fmt.Errorf("create TUN interface: %w", err)
	}
	defer func() {
		if !success {
			tun.Close()
		}
	}()
	var commands [][]string
	if runtime.GOOS == "darwin" {
		commands = [][]string{{"/sbin/ifconfig", tun.Name(), "inet6", "add", p.ClientParameters.Address + "/64"}, {"/sbin/ifconfig", tun.Name(), "mtu", strconv.Itoa(p.ClientParameters.MTU), "up"}}
	} else {
		commands = [][]string{{"ip", "-6", "addr", "add", p.ClientParameters.Address + "/64", "dev", tun.Name()}, {"ip", "link", "set", "dev", tun.Name(), "mtu", strconv.Itoa(p.ClientParameters.MTU), "up"}}
	}
	for _, command := range commands {
		if output, err := exec.CommandContext(ctx, command[0], command[1:]...).CombinedOutput(); err != nil {
			return "", 0, nil, fmt.Errorf("configure tunnel (root or CAP_NET_ADMIN required): %w: %s", err, output)
		}
	}
	if err := ctx.Err(); err != nil {
		return "", 0, nil, err
	}
	usb.live()
	t := &ownedTunnel{usb: usb, tun: tun}
	go func() { _ = forwardIPv6(tun, usb); t.Close() }()
	go func() {
		packet := make([]byte, p.ClientParameters.MTU+40)
		for {
			n, err := tun.Read(packet)
			if err != nil {
				break
			}
			if err = writeAll(usb, packet[:n]); err != nil {
				break
			}
		}
		t.Close()
	}()
	success = true
	return p.ServerAddress, p.ServerRSDPort, t, nil
}
