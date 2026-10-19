package hid

import (
	"context"
	"os"
	"sync"
	"time"
)

const (
	serialProbeTimeout      = time.Second
	serialProbeSettle       = 100 * time.Millisecond
	serialProbeReplyTimeout = 250 * time.Millisecond
	serialProbeRetryDelay   = 50 * time.Millisecond
)

func probeSerialPort(port string, baud uint32, query func(serialReadWriter) bool) bool {
	return runSerialProbe(serialProbeTimeout,
		func() (serialReadWriter, error) { return openProbeSerialFile(port) },
		func(rw serialReadWriter) error { return setProbeTermios(rw.(*os.File), baud) }, query)
}

// Bound the whole discovery attempt, including open/configuration and writes.
// Cancel pending I/O by closing the owned port. An open that finishes after
// cancellation is closed without configuring or querying it.
func runSerialProbe(timeout time.Duration, open func() (serialReadWriter, error),
	configure func(serialReadWriter) error, query func(serialReadWriter) bool) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	result := make(chan bool, 1)
	go func() {
		ok := false
		defer func() { result <- ok }()
		rw, err := open()
		if err != nil {
			return
		}
		deadline, _ := ctx.Deadline()
		owned := &serialProbeIO{serialReadWriter: rw, deadline: deadline}
		stop := context.AfterFunc(ctx, func() { _ = owned.Close() })
		defer stop()
		defer owned.Close()
		if ctx.Err() != nil || configure(rw) != nil {
			return
		}
		timer := time.NewTimer(serialProbeSettle)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return
		}
		if ctx.Err() == nil {
			ok = query(owned) && ctx.Err() == nil
		}
	}()
	select {
	case ok := <-result:
		return ok && ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}

type serialProbeIO struct {
	serialReadWriter
	deadline time.Time
	once     sync.Once
	closeErr error
}

func (p *serialProbeIO) Close() error {
	p.once.Do(func() { p.closeErr = p.serialReadWriter.Close() })
	return p.closeErr
}

func (p *serialProbeIO) Write(data []byte) (int, error) {
	if !time.Now().Before(p.deadline) {
		return 0, os.ErrDeadlineExceeded
	}
	return p.serialReadWriter.Write(data)
}

func (p *serialProbeIO) SetReadDeadline(t time.Time) error {
	if t.IsZero() || t.After(p.deadline) {
		t = p.deadline
	}
	return p.serialReadWriter.SetReadDeadline(t)
}

func (p *serialProbeIO) SetWriteDeadline(t time.Time) error {
	if t.IsZero() || t.After(p.deadline) {
		t = p.deadline
	}
	return p.serialReadWriter.SetWriteDeadline(t)
}
