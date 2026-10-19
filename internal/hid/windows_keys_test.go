package hid

import "testing"

func TestWindowsPhysicalKeys(t *testing.T) {
	for _, tc := range []struct {
		usage uint8
		scan  uint16
	}{{4, 0x1e}, {40, 0x1c}, {88, 0x11c}, {224, 0x1d}, {228, 0x11d}, {79, 0x14d}, {0, 0}} {
		if got := windowsScanCode(tc.usage); got != tc.scan {
			t.Fatalf("usage %d: %x != %x", tc.usage, got, tc.scan)
		}
	}
}
