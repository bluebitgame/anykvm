package hid

import "fmt"

// --- AnyKVM serial HID --------------------------------------------------

func openSerialHID(absMouse bool) (hidi, error) { return openSerialAdapter(absMouse, false) }

func openSerialAdapter(absMouse, requireBLE bool) (hidi, error) {
	for _, port := range serialHIDCandidates() {
		d, err := openSerialHIDAt(port)
		if err != nil {
			continue
		}
		n, ok := d.(*serialHIDDevice)
		if !ok {
			d.Close()
			continue
		}
		if requireBLE && n.lastStatus().Capabilities&capabilityBLE == 0 {
			n.closeLink()
			continue
		}
		if err := configureSerialSession(n, absMouse, SlotUnset); err != nil {
			d.Close()
			continue
		}
		return d, nil
	}
	return nil, fmt.Errorf("hid: no AnyKVM serial HID adapter found")
}

func openSerialHIDAt(port string) (hidi, error) {
	d, err := serialConnect(port)
	if err != nil {
		return nil, err
	}
	return d, nil
}
