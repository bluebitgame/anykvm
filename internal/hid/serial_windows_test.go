//go:build windows

package hid

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

func TestCOMPortsOpenExclusivelyWithNativeDeviceNames(t *testing.T) {
	original := createSerialFile
	t.Cleanup(func() { createSerialFile = original })
	for _, port := range []string{"COM5", "COM10", `\\.\COM256`} {
		called := false
		createSerialFile = func(name *uint16, access, share uint32, sa *windows.SecurityAttributes, disposition, flags uint32, template windows.Handle) (windows.Handle, error) {
			called = true
			want := port
			if port[0] != '\\' {
				want = `\\.\` + port
			}
			if got := windows.UTF16PtrToString(name); got != want {
				t.Fatalf("device path = %q, want %q", got, want)
			}
			if access != windows.GENERIC_READ|windows.GENERIC_WRITE || share != 0 || sa != nil || disposition != windows.OPEN_EXISTING || flags != windows.FILE_ATTRIBUTE_NORMAL || template != 0 {
				t.Fatalf("COM port must open exclusively for read/write without inheritance: access=%x share=%x disposition=%x flags=%x", access, share, disposition, flags)
			}
			return windows.InvalidHandle, windows.ERROR_FILE_NOT_FOUND
		}
		file, err := openHIDSerialFile(port)
		if !called || file != nil || !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			t.Fatalf("open %q: file=%v error=%v called=%v", port, file, err, called)
		}
	}
}
