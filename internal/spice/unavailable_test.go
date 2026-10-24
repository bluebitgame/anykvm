//go:build !linux || !cgo || !spice

package spice

import (
	"strings"
	"testing"
)

func TestUnavailableSourceDoesNotFallBackToLocalCapture(t *testing.T) {
	for _, raw := range []string{"spice:/tmp/vm.sock", "spice://localhost:5900"} {
		device, ok := NewCapture(raw, Options{})
		if !ok {
			t.Fatal("SPICE source not recognized")
		}
		if err := device.Open(); err == nil || !strings.Contains(err.Error(), "Linux, CGO and -tags spice") {
			t.Fatalf("unexpected unsupported-platform behavior: %v", err)
		}
	}
}
