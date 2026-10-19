//go:build !windows

package hid

import (
	"fmt"
	"os"
	"time"
)

// openCH9329 finds and opens a CH9329 device. absMouse selects absolute (true)
// or relative (false) reports.
func openCH9329(absMouse bool) (hidi, error) {
	if absMouse {
		return openCH9329Abs()
	}
	return openCH9329Rel()
}

func openCH9329Abs() (hidi, error) {
	port, err := findCH9329()
	if err != nil {
		return nil, err
	}
	return openCH9329Port(port, true)
}

func openCH9329Rel() (hidi, error) {
	port, err := findCH9329()
	if err != nil {
		return nil, err
	}
	return openCH9329Port(port, false)
}

func openCH9329Port(port string, absolute bool) (hidi, error) {
	f, err := os.OpenFile(port, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("ch9329: open %s: %w", port, err)
	}
	if err := setRawTermios(f, baud9600); err != nil {
		f.Close()
		return nil, fmt.Errorf("ch9329: configure %s: %w", port, err)
	}
	time.Sleep(100 * time.Millisecond)

	base := newCH9329Base(f, absolute)
	if absolute {
		return &ch9329AbsDevice{ch9329Base: base}, nil
	}
	return &ch9329RelDevice{ch9329Base: base}, nil
}
