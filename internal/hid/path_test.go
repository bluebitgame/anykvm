package hid

import "testing"

func TestParseHIDSpec(t *testing.T) {
	tests := []struct {
		in     string
		typ    string
		device string
		slot   int
	}{
		{"ble:/dev/cu.usbmodem14101:0", "ble", "/dev/cu.usbmodem14101", 0},
		{"ble:cu.usbmodem14101:0", "ble", "/dev/cu.usbmodem14101", 0},
		{"ble:cu.usbmodem14101:3", "ble", "/dev/cu.usbmodem14101", 3},
		{"ble:cu.usbmodem14101", "ble", "/dev/cu.usbmodem14101", SlotUnset},
		{"nrf52840:ttyACM0:1", "nrf52840", "/dev/ttyACM0", 1},
		{"ch9329:ttyUSB0", "ch9329", "/dev/ttyUSB0", SlotUnset},
		{"hid:ttyUSB0", "hid", "/dev/ttyUSB0", SlotUnset},
		{"ttyUSB0", "hid", "/dev/ttyUSB0", SlotUnset},
		{"local", "local", "", SlotUnset},
		{"auto", "auto", "", SlotUnset},
		{"", "auto", "", SlotUnset},
	}
	for _, tc := range tests {
		spec := ParseHIDSpec(tc.in)
		if spec.Type != tc.typ || spec.Device != tc.device || spec.Slot != tc.slot {
			t.Errorf("ParseHIDSpec(%q) = %+v, want type=%q device=%q slot=%d",
				tc.in, spec, tc.typ, tc.device, tc.slot)
		}
	}
}

func TestSpecOrAuto(t *testing.T) {
	spec := SpecOrAuto("")
	if spec.Type != "auto" {
		t.Fatalf("got %q", spec.Type)
	}
	spec = SpecOrAuto("ble:cu.usbmodem14101:0")
	if spec.Type != "ble" || spec.Slot != 0 {
		t.Fatalf("got %+v", spec)
	}
}

func TestParsePairSpec(t *testing.T) {
	slot, dev, err := ParsePairSpec("ble:cu.usbmodem14101:0")
	if err != nil || slot != 0 || dev != "/dev/cu.usbmodem14101" {
		t.Fatalf("got slot=%d dev=%q err=%v", slot, dev, err)
	}
	slot, dev, err = ParsePairSpec("ble:cu.usbmodem14101")
	if err != nil || slot != 0 || dev != "/dev/cu.usbmodem14101" {
		t.Fatalf("default slot: got slot=%d dev=%q err=%v", slot, dev, err)
	}
	if _, _, err := ParsePairSpec("local"); err == nil {
		t.Fatal("expected error for non-BLE type")
	}
}
