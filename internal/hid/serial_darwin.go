//go:build darwin

package hid

import (
	"os"
	"syscall"
	"unsafe"
)

const (
	baud115200 = 115200
	baud9600   = 9600
)

func setProbeTermios(f *os.File, baud uint32) error   { return setRawTermios(f, baud) }
func setControlTermios(f *os.File, baud uint32) error { return setRawTermios(f, baud) }

func setRawTermios(f *os.File, baud uint32) error {
	fd := int(f.Fd())
	var t syscall.Termios

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCGETA, uintptr(unsafe.Pointer(&t))); errno != 0 {
		return errno
	}

	t.Ispeed = uint64(baud)
	t.Ospeed = uint64(baud)

	t.Cflag &^= syscall.PARENB | syscall.CSTOPB | syscall.CSIZE | syscall.PARODD
	t.Cflag |= syscall.CS8 | syscall.CREAD | syscall.CLOCAL
	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	t.Oflag &^= syscall.OPOST
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	// VMIN=1 makes Read block until a byte arrives; on macOS that ignores
	// SetReadDeadline and hangs drainSerial/readLoop when the port is idle.
	t.Cc[syscall.VMIN] = 0
	t.Cc[syscall.VTIME] = 1 // 100ms inter-byte timer; idle read returns promptly

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCSETA, uintptr(unsafe.Pointer(&t))); errno != 0 {
		return errno
	}
	return setSerialDTR(f)
}

func setSerialDTR(f *os.File) error {
	fd := int(f.Fd())
	var status uint
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCMGET, uintptr(unsafe.Pointer(&status))); errno != 0 {
		return errno
	}
	status |= syscall.TIOCM_DTR
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCMSET, uintptr(unsafe.Pointer(&status)))
	if errno != 0 {
		return errno
	}
	return nil
}
