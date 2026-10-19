package app

import (
	"testing"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"anykvm/internal/coredevice"
)

func TestCoreDeviceHybridConfigAndBackend(t *testing.T) {
	m := TargetConfig{Name: "hybrid", Video: VideoConfig{Source: "ios-usb:iPhone"}, HID: HIDConfig{Device: "coredevice:test-phone"}}
	if err := validateCoreDeviceMachine(m); err != nil {
		t.Fatal(err)
	}
	d, android := newCaptureSession(m)
	if _, ok := d.(*coredevice.Hybrid); !ok || android != nil {
		t.Fatal("hybrid not selected")
	}
	if _, ok := d.(bridge.HIDSender); !ok {
		t.Fatal("hybrid missing input")
	}
	if p, ok := d.(capture.AudioProvider); !ok || p.AudioDevice() == nil {
		t.Fatal("hybrid missing USB source audio")
	}
	for _, bad := range []TargetConfig{
		{Video: VideoConfig{Source: "camera"}, HID: HIDConfig{Device: "coredevice"}},
		{Video: VideoConfig{Source: "ios-usb"}, HID: HIDConfig{Device: "coredevice:"}},
		{Video: VideoConfig{Source: "coredevice"}, HID: HIDConfig{Device: "coredevice"}},
	} {
		if err := validateCoreDeviceMachine(bad); err == nil {
			t.Fatal("invalid combination accepted", bad)
		}
	}
}
