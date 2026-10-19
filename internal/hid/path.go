package hid

import (
	"fmt"
	"strconv"
	"strings"
)

// SlotUnset means no explicit BLE profile slot was given; use the dongle's
// current active slot (firmware default is 0 after reboot).
const SlotUnset = -1

var hidTypeKeywords = map[string]struct{}{
	"auto":      {},
	"local":     {},
	"ch9329":    {},
	"hid":       {},
	"gadget":    {},
	"bluetooth": {},
	"ble":       {},
	"nrf52840":  {},
}

// HIDSpec is the parsed form of -hid / config device strings.
type HIDSpec struct {
	Type   string
	Device string
	Slot   int // 0-3, or SlotUnset
}

// ParseHIDSpec parses strings such as:
//   - ble:cu.usbmodem14101:0
//   - nrf52840:ttyACM0:2
//   - ch9329:ttyUSB0
//   - hid:ttyUSB0
//   - local
//   - auto
func ParseHIDSpec(s string) HIDSpec {
	if s == "" {
		return HIDSpec{Type: "auto", Slot: SlotUnset}
	}

	i := strings.IndexByte(s, ':')
	if i < 0 {
		if _, ok := hidTypeKeywords[s]; ok {
			return HIDSpec{Type: s, Slot: SlotUnset}
		}
		return HIDSpec{Type: "hid", Device: normalizeDevPath(s), Slot: SlotUnset}
	}

	typ := s[:i]
	rest := s[i+1:]
	spec := HIDSpec{Type: typ, Slot: SlotUnset}

	if typ == "ble" || typ == "nrf52840" {
		if path, slot, ok := splitBLEDeviceSlot(rest); ok {
			spec.Device = path
			spec.Slot = slot
			return spec
		}
	}

	spec.Device = normalizeDevPath(rest)
	return spec
}

// SpecOrAuto parses a config device string, defaulting empty to auto.
func SpecOrAuto(s string) HIDSpec {
	if s == "" {
		return ParseHIDSpec("auto")
	}
	return ParseHIDSpec(s)
}

// normalizeDevPath converts a shorthand serial device to the platform's native
// spelling. In particular, Windows COM ports must never become /dev/COMx.
func normalizeDevPath(path string) string {
	return platformDevicePath(path)
}

// splitBLEDeviceSlot extracts an optional trailing :N slot from a device path.
func splitBLEDeviceSlot(device string) (path string, slot int, ok bool) {
	slot = SlotUnset
	j := strings.LastIndexByte(device, ':')
	if j <= 0 {
		return normalizeDevPath(device), SlotUnset, false
	}
	n, err := strconv.Atoi(device[j+1:])
	if err != nil || n < 0 || n > 3 {
		return normalizeDevPath(device), SlotUnset, false
	}
	return normalizeDevPath(device[:j]), n, true
}

// ParsePairSpec parses -pair values such as ble:cu.usbmodem14101:0.
// Slot defaults to 0 when omitted from the path.
func ParsePairSpec(s string) (slot int, device string, err error) {
	if s == "" {
		return 0, "", fmt.Errorf("pair: spec required (e.g. ble:cu.usbmodem14101:0)")
	}
	spec := ParseHIDSpec(s)
	if spec.Type != "ble" && spec.Type != "nrf52840" {
		return 0, "", fmt.Errorf("pair: want ble:DEVICE[:N] or nrf52840:DEVICE[:N], got %q", s)
	}
	if spec.Device == "" {
		return 0, "", fmt.Errorf("pair: device path required in %q", s)
	}
	slot = spec.Slot
	if slot < 0 {
		slot = 0
	}
	return slot, spec.Device, nil
}
