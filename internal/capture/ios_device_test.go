package capture

import "testing"

func TestIOSUSBDeviceSelection(t *testing.T) {
	for _, tc := range []struct {
		device   string
		want     bool
		selector string
	}{
		{"ios-usb", true, ""}, {"ios-usb:iPad", true, "iPad"},
		{"avf", false, ""}, {"avf:iPad", false, ""},
		{"ios", false, ""}, {"ios:iPad", false, ""},
		{"coredevice", false, ""}, {"coredevice:abc", false, ""},
		{"avfoundation", false, ""},
	} {
		if got := IsIOSDevice(tc.device); got != tc.want {
			t.Errorf("IsIOSDevice(%q)=%v", tc.device, got)
		}
		if tc.want && iosSelector(tc.device) != tc.selector {
			t.Errorf("wrong iOS USB selector for %q", tc.device)
		}
	}
}

func TestIOSUSBPreservesCaptureMode(t *testing.T) {
	for _, selector := range []string{"ios-usb", "ios-usb:iPad", "Capture Card"} {
		dev := &smartDevice{deviceID: selector}
		dev.SetVideoStreamOptions(&VideoStreamOptions{Width: 1920, Height: 1080, FPS: 30, Bitrate: 24000})
		if !dev.PreserveCaptureMode() {
			t.Fatalf("%s allows client resolution to resize the source", selector)
		}
	}
}
