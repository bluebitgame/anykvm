package coredevice

import (
	"fmt"

	"anykvm/internal/capture"
	"anykvm/internal/logging"
)

// ValidateSource checks the source selector and protocol-specific settings.
func ValidateSource(video, input string, options Options) error {
	if !IsDevice(video) && !IsDevice(input) {
		return nil
	}
	selector := video
	if !IsDevice(selector) {
		if !capture.IsIOSDevice(video) {
			return fmt.Errorf("standalone CoreDevice HID requires video.source ios-usb[:name]")
		}
		selector = input
	}
	if _, err := DeviceUDID(selector); err != nil {
		return err
	}
	if err := options.Validate(); err != nil {
		return err
	}
	if IsDevice(video) && input != "" && input != "auto" {
		return fmt.Errorf("CoreDevice supplies input; omit hid.device")
	}
	return nil
}

// NewCapture selects direct CoreDevice or independent USB video with CoreDevice input.
// A matching but invalid source preserves its error rather than discovering a camera.
func NewCapture(video, input string, options Options) (capture.Device, bool) {
	if !IsDevice(video) && !IsDevice(input) {
		return nil, false
	}
	if err := ValidateSource(video, input, options); err != nil {
		return capture.Failed(err), true
	}
	if IsDevice(input) {
		client, err := NewInputClient(input, options)
		if err != nil {
			return capture.Failed(err), true
		}
		logging.Infof("AnyKVM: ios-usb video capture with standalone CoreDevice HID configured")
		return NewHybrid(capture.NewDevice(video), client), true
	}
	d, err := NewClient(video, options)
	if err != nil {
		return capture.Failed(err), true
	}
	logging.Infof("AnyKVM: direct CoreDevice capture and HID configured")
	return d, true
}
