//go:build darwin

package discovery

import (
	"io/fs"
	"reflect"
	"testing"
	"testing/fstest"
)

func TestMacSerialPortsSkipsDialInAndBluetooth(t *testing.T) {
	entries, err := fs.ReadDir(fstest.MapFS{
		"cu.BLTH": {}, "tty.BLTH": {},
		"cu.Bluetooth-Incoming-Port": {}, "tty.Bluetooth-Incoming-Port": {},
		"cu.usbmodem123": {}, "tty.usbmodem123": {},
		"cu.wchusbserial123": {}, "tty.wchusbserial123": {},
		"cu.SLAB_USBtoUART": {},
	}, ".")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/dev/cu.SLAB_USBtoUART", "/dev/cu.usbmodem123", "/dev/cu.wchusbserial123"}
	if got := macSerialPorts(entries); !reflect.DeepEqual(got, want) {
		t.Fatalf("ports = %v; want %v", got, want)
	}
}
