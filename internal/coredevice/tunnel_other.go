//go:build !darwin && !linux

package coredevice

import (
	"context"
	"fmt"
	"github.com/danielpaulus/go-ios/ios"
	"io"
)

func openUSBTunnel(context.Context, ios.DeviceEntry) (string, int, io.Closer, error) {
	return "", 0, nil, fmt.Errorf("USB tunnel creation is supported on macOS/Linux; set coredevice.rsd to an existing kernel tunnel on this platform")
}
