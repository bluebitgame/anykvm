package ard

import (
	"anykvm/internal/capture"
	"anykvm/internal/logging"
)

// ValidateSource checks the source selector and protocol-specific settings.
func ValidateSource(device string, options Options) error {
	if !IsDevice(device) {
		return nil
	}
	if _, err := ParseAddress(device); err != nil {
		return err
	}
	if err := options.Validate(); err != nil {
		return err
	}
	return nil
}

// NewCapture selects this protocol and preserves setup errors in a failed source.
func NewCapture(device string, options Options) (capture.Device, bool) {
	if !IsDevice(device) {
		return nil, false
	}
	d, err := NewClient(device, options)
	if err != nil {
		logging.Infof("AnyKVM: ARD configuration error: %v", err)
		return capture.Failed(err), true
	}
	logging.Infof("AnyKVM: ARD capture source configured")
	return d, true
}
