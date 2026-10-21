//go:build !linux || !cgo || !spice

package spice

import (
	"anykvm/internal/capture"
	"fmt"
)

func newDevice(string, Options) capture.Device {
	return capture.Failed(fmt.Errorf("SPICE GPU upstream requires Linux, CGO and -tags spice; install spice-client-glib, EGL, GBM and libdrm development packages"))
}
