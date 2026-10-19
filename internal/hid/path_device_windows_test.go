//go:build windows

package hid

import "testing"

func TestWindowsCOMPathStaysNative(t *testing.T) {
	if got := ParseHIDSpec("hid:COM5").Device; got != "COM5" {
		t.Fatalf("COM path = %q, want COM5", got)
	}
}
