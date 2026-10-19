//go:build linux

package hid

import (
	"encoding/binary"
	"fmt"
	"os"
	"time"

	"github.com/godbus/dbus/v5"
)

// bleHID uses Linux BlueZ to act as a Bluetooth HID keyboard + mouse.
// Requires: bluez, btmgmt, and bluetoothd running.
// Targets must pair with this device before HID reports are accepted.
type bleHID struct {
	conn   *dbus.Conn
	kbdChr *dbus.Object
	msChr  *dbus.Object
}

// Bluetooth HID report reference UUIDs
const (
	hidServiceUUID = "00001812-0000-1000-8000-00805f9b34fb"
	hidReportMap   = "00002a4b-0000-1000-8000-00805f9b34fb"
	hidReportInKbd = "00002a4d-0000-1000-8000-00805f9b34fb" // keyboard input report
	hidReportInMs  = "00002a4d-0000-1000-8000-00805f9b34fb" // same UUID, different service
	gapAppearance  = "00002a01-0000-1000-8000-00805f9b34fb"
)

const bluetoothSetupMsg = `
Bluetooth HID requires manual pairing first:
  1. sudo btmgmt power on
  2. sudo btmgmt connectable on
  3. sudo btmgmt advertising on
  4. On target computer, pair with "AnyKVM BT" as keyboard
`

func openBluetooth() (hidi, error) {
	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, fmt.Errorf("bluetooth: dbus: %w (is bluetoothd running?)", err)
	}

	bh := &bleHID{conn: conn}

	if err := bh.setupHID(); err != nil {
		conn.Close()
		return nil, err
	}

	fmt.Fprintf(os.Stderr, bluetoothSetupMsg)
	return bh, nil
}

func (b *bleHID) setupHID() error {
	// Get BlueZ adapter
	obj := b.conn.Object("org.bluez", "/org/bluez/hci0")
	if obj == nil {
		return fmt.Errorf("bluetooth: no adapter hci0 found")
	}

	// Register HID profile via BlueZ D-Bus API
	// This would register as a BLE HID device
	// Full implementation requires BlueZ HID profile support

	return fmt.Errorf("bluetooth: BlueZ HID registration not yet implemented (needs BlueZ HID profile plugin)")
}

func (b *bleHID) Close() error {
	return b.conn.Close()
}

func (b *bleHID) sendKbdReport(report []byte) error {
	if b.kbdChr == nil {
		return nil // no client connected yet
	}
	return b.kbdChr.Call("org.bluez.GattCharacteristic1.WriteValue", 0, report).Err
}

func (b *bleHID) sendMsReport(report []byte) error {
	if b.msChr == nil {
		return nil
	}
	return b.msChr.Call("org.bluez.GattCharacteristic1.WriteValue", 0, report).Err
}

func (b *bleHID) KeyDown(keycode uint8) error {
	report := []byte{0, 0, keycode, 0, 0, 0, 0, 0}
	return b.sendKbdReport(report)
}

func (b *bleHID) KeyUp() error {
	return b.sendKbdReport([]byte{0, 0, 0, 0, 0, 0, 0, 0})
}

func (b *bleHID) KeyRelease(keycode uint8) error {
	return b.KeyUp()
}

func (b *bleHID) MouseMove(dx, dy int) error {
	data := make([]byte, 5)
	data[1] = uint8(int8(dx))
	data[2] = uint8(int8(dy))
	binary.LittleEndian.PutUint16(data[3:], 0)
	return b.sendMsReport(data[:4])
}

// MouseMoveRel is identical to MouseMove for the BlueZ BLE backend: the HID
// report is already relative.
func (b *bleHID) MouseMoveRel(dx, dy int) error {
	return b.MouseMove(dx, dy)
}

func (b *bleHID) MouseButton(button int, down bool) error {
	var btn byte
	switch button {
	case 1:
		btn = 1
	case 2:
		btn = 2
	case 3:
		btn = 4
	}
	var report [4]byte
	if down {
		report[0] = btn
	}
	return b.sendMsReport(report[:])
}

func (b *bleHID) MouseWheel(delta int) error {
	return nil // wheel support depends on report descriptor
}

// keepalive prevents BlueZ from disconnecting the HID service
func (b *bleHID) keepalive() {
	for {
		time.Sleep(5 * time.Second)
		b.conn.Object("org.bluez", "/").Call("org.freedesktop.DBus.Peer.Ping", 0)
	}
}
