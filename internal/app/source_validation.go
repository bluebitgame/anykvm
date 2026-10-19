package app

import (
	"fmt"

	"anykvm/internal/airplay"
	"anykvm/internal/ard"
	"anykvm/internal/coredevice"
)

// Attach target names to module validation errors for configuration diagnostics.
func validateARDMachine(m TargetConfig) error {
	if err := ard.ValidateSource(m.Video.Source, m.ARD); err != nil {
		return fmt.Errorf("target %q: %w", m.Name, err)
	}
	return nil
}
func validateAirPlayMachine(m TargetConfig) error {
	if err := airplay.ValidateSource(m.Video.Source, m.AirPlay); err != nil {
		return fmt.Errorf("target %q: %w", m.Name, err)
	}
	return nil
}
func validateCoreDeviceMachine(m TargetConfig) error {
	if err := coredevice.ValidateSource(m.Video.Source, m.HID.Device, m.CoreDevice); err != nil {
		return fmt.Errorf("target %q: %w", m.Name, err)
	}
	return nil
}
