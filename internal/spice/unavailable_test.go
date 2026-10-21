//go:build !linux || !cgo || !spice

package spice

import (
	"strings"
	"testing"
)

func TestUnavailableSourceDoesNotFallBackToLocalCapture(t *testing.T) {
	device, ok := NewCapture("spice:/tmp/vm.sock", Options{})
	if !ok {
		t.Fatal("SPICE source not recognized")
	}
	if err := device.Open(); err == nil || !strings.Contains(err.Error(), "Linux, CGO and -tags spice") {
		t.Fatalf("unexpected unsupported-platform behavior: %v", err)
	}
}
