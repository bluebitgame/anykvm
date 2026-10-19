//go:build windows

package hid

import (
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"os"
	"sort"
	"strings"
	"unsafe"
)

const (
	baud115200 = 115200
	baud9600   = 9600
)

var createSerialFile = windows.CreateFile

func openWindowsSerial(port string, baud uint32) (*os.File, error) {
	f, err := openHIDSerialFile(port)
	if err != nil {
		return nil, err
	}
	if err = setRawTermios(f, baud); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
func setRawTermios(f *os.File, baud uint32) error {
	h := windows.Handle(f.Fd())
	dcb := windows.DCB{DCBlength: uint32(unsafe.Sizeof(windows.DCB{}))}
	if err := windows.GetCommState(h, &dcb); err != nil {
		return err
	}
	dcb.BaudRate = baud
	dcb.Flags = 1 | (1 << 4) // binary, DTR enabled; no software/hardware flow control
	dcb.ByteSize, dcb.Parity, dcb.StopBits = 8, 0, 0
	if err := windows.SetCommState(h, &dcb); err != nil {
		return err
	}
	return windows.SetCommTimeouts(h, &windows.CommTimeouts{ReadIntervalTimeout: 0xffffffff, ReadTotalTimeoutMultiplier: 0xffffffff, ReadTotalTimeoutConstant: 100, WriteTotalTimeoutConstant: 1000})
}
func setSerialDTR(f *os.File) error {
	return windows.EscapeCommFunction(windows.Handle(f.Fd()), windows.SETDTR)
}

func setControlTermios(f *os.File, baud uint32) error {
	if err := setRawTermios(f, baud); err != nil {
		return err
	}
	// Go's synchronous os.File serializes Read and Write on Windows. Poll for
	// available bytes instead of holding that lock for a 100 ms idle read; the
	// shared control reader already backs off on EOF. This does not affect the
	// CH9329 reader, which uses the ordinary raw serial timeouts above.
	return windows.SetCommTimeouts(windows.Handle(f.Fd()), &windows.CommTimeouts{
		ReadIntervalTimeout: 0xffffffff, WriteTotalTimeoutConstant: 1000,
	})
}

func setProbeTermios(f *os.File, baud uint32) error {
	if err := setControlTermios(f, baud); err != nil {
		return err
	}
	// Discovery also needs reads to return promptly so an exclusive COM port
	// is released before trying the next protocol after a timeout.
	return windows.SetCommTimeouts(windows.Handle(f.Fd()), &windows.CommTimeouts{
		ReadIntervalTimeout: 0xffffffff, WriteTotalTimeoutConstant: 100,
	})
}

func openProbeSerialFile(port string) (*os.File, error) { return openHIDSerialFile(port) }

// ListSerialPorts enumerates registered COM names without opening devices.
func ListSerialPorts() []string {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DEVICEMAP\SERIALCOMM`, registry.QUERY_VALUE)
	if err != nil {
		return nil
	}
	defer key.Close()
	names, _ := key.ReadValueNames(-1)
	var ports []string
	for _, name := range names {
		if port, _, err := key.GetStringValue(name); err == nil {
			ports = append(ports, port)
		}
	}
	sort.Strings(ports)
	return ports
}
func serialHIDCandidates() []string { return ListSerialPorts() }
func openHIDSerialFile(port string) (*os.File, error) {
	if strings.HasPrefix(strings.ToUpper(port), "COM") {
		port = `\\.\` + port
	}
	name, err := windows.UTF16PtrFromString(port)
	if err != nil {
		return nil, err
	}
	// Win32 communications devices require exclusive access (share mode 0).
	// os.OpenFile requests FILE_SHARE_READ|FILE_SHARE_WRITE and is unsuitable.
	handle, err := createSerialFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: port, Err: err}
	}
	return os.NewFile(uintptr(handle), port), nil
}
