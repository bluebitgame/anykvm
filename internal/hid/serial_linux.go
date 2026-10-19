//go:build linux

package hid

import (
	"golang.org/x/sys/unix"
	"os"
	"syscall"
	"unsafe"
)

const (
	baud115200 = 0x1002 // B115200
	baud9600   = 0x000D // B9600
)

func setRawTermios(f *os.File, baud uint32) error {
	return setRawTermiosMode(f, baud, false)
}

func setProbeTermios(f *os.File, baud uint32) error {
	return setRawTermiosMode(f, baud, true)
}

func setControlTermios(f *os.File, baud uint32) error {
	return setRawTermiosMode(f, baud, true)
}

func setRawTermiosMode(f *os.File, baud uint32, probing bool) error {
	fd := int(f.Fd())
	var t syscall.Termios

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCGETS, uintptr(unsafe.Pointer(&t))); errno != 0 {
		return errno
	}

	t.Ispeed = baud
	t.Ospeed = baud

	// TCSETS encodes standard Linux baud rates in CBAUD, not just Ispeed and
	// Ospeed. Clear a stale independent input speed as well (for example 1200).
	t.Cflag &^= syscall.PARENB | syscall.PARODD | syscall.CSTOPB | syscall.CSIZE | unix.CRTSCTS | unix.CBAUD | unix.CIBAUD
	t.Cflag |= baud | syscall.CS8 | syscall.CREAD | syscall.CLOCAL
	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON | syscall.IXOFF | syscall.IXANY
	t.Oflag &^= syscall.OPOST
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	if probing {
		// Fd() switches the file to blocking mode. A kernel idle timeout
		// bounds both discovery and the initial control-port drain.
		t.Cc[syscall.VMIN] = 0
		t.Cc[syscall.VTIME] = 1
	}

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCSETS, uintptr(unsafe.Pointer(&t))); errno != 0 {
		return errno
	}
	return setSerialDTR(f)
}

func setSerialDTR(f *os.File) error {
	fd := int(f.Fd())
	var status int
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
