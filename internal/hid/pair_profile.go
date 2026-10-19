package hid

import (
	"fmt"
	"log"
	"time"
)

// PairProfile advertises and waits for a BLE target to bond.
func PairProfile(pairSpec, mouseMode string) error {
	slot, devPath, err := ParsePairSpec(pairSpec)
	if err != nil {
		return err
	}

	d, err := OpenConfigured("ble", devPath, mouseMode, slot)
	if err != nil {
		return fmt.Errorf("open BLE dongle: %w", err)
	}
	defer d.Close()

	ble, ok := d.BLE()
	if !ok {
		return fmt.Errorf("opened device does not support BLE profile management")
	}

	log.Printf("AnyKVM: pairing slot %d (mouse mode: %s)...", slot, mouseMode)

	if err := ble.Pair(slot); err != nil {
		return fmt.Errorf("pair: %w", err)
	}

	log.Printf("AnyKVM: slot %d is now advertising — pair with \"AnyKVM\" on the target device", slot)
	log.Printf("AnyKVM: waiting for bond (USB serial may reconnect after pairing)...")

	var st ProfileStatus
	d, st, err = WaitBonded(d, devPath, 5*time.Minute)
	if err != nil {
		return err
	}
	log.Printf("AnyKVM: slot %d paired successfully (mode=%s)", slot, st.MouseMode)
	return nil
}
