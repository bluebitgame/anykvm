//go:build linux

package hid

import (
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func linuxSerialPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	m := os.NewFile(uintptr(master), "ptmx")
	t.Cleanup(func() { m.Close() })
	if err := unix.IoctlSetPointerInt(master, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(master, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	f, err := openHIDSerialFile(fmt.Sprintf("/dev/pts/%d", n))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return m, f
}

func TestLinuxControlSerialResetsBaudAndBoundsIdleRead(t *testing.T) {
	master, port := linuxSerialPTY(t)
	fd := int(port.Fd())
	settings, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	settings.Cflag = (settings.Cflag &^ unix.CBAUD) | unix.B1200
	settings.Iflag |= unix.IXON | unix.IXOFF | unix.IXANY
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, settings); err != nil {
		t.Fatal(err)
	}
	// A PTY has no modem-control pins. Its ENOTTY comes after applying raw
	// settings; a real CDC adapter also receives DTR in this same code path.
	if err := setControlTermios(port, baud115200); err != nil && !errors.Is(err, unix.ENOTTY) {
		t.Fatal(err)
	}
	settings, err = unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Cflag&unix.CBAUD != unix.B115200 || settings.Cflag&unix.CIBAUD != 0 {
		t.Fatalf("stale baud survived configuration: cflag=%x", settings.Cflag)
	}
	if settings.Iflag&(unix.IXON|unix.IXOFF|unix.IXANY) != 0 || settings.Lflag&(unix.ICANON|unix.ECHO) != 0 {
		t.Fatal("serial control bytes can be consumed by line processing")
	}
	if settings.Cc[unix.VMIN] != 0 || settings.Cc[unix.VTIME] != 1 {
		t.Fatalf("unbounded idle read: MIN=%d TIME=%d", settings.Cc[unix.VMIN], settings.Cc[unix.VTIME])
	}
	done := make(chan error, 1)
	go func() {
		var data [8]byte
		n, err := port.Read(data[:])
		if n != 0 || (err != nil && err != io.EOF) {
			done <- fmt.Errorf("idle read returned %d bytes, %v", n, err)
			return
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		// Also release the reader if this test regresses to VMIN=1.
		_, _ = master.Write([]byte{0})
		t.Fatal("idle control port blocks instead of returning within 100 ms")
	}
}
