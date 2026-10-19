package bridge

import (
	"anykvm/internal/capture"
	"errors"
	"testing"
	"time"
)

type recoveringCapture struct {
	blockingCapture
	idle, expected           bool
	failures                 int
	failed                   chan struct{}
	openStarted, openRelease chan struct{}
}

func (*recoveringCapture) CanRecoverCapture() bool             { return true }
func (d *recoveringCapture) CaptureIdleStatus() (bool, string) { return d.expected, "test idle" }
func (d *recoveringCapture) OpenWithFormat(w, h, fps int) error {
	d.mu.Lock()
	reopening := d.opens > 0
	if d.opens > 0 && d.failures > 0 {
		d.failures--
		d.mu.Unlock()
		return errors.New("desktop still locked")
	}
	d.mu.Unlock()
	if reopening && d.openStarted != nil {
		close(d.openStarted)
		<-d.openRelease
	}
	return d.blockingCapture.OpenWithFormat(w, h, fps)
}
func (d *recoveringCapture) Capture() (*capture.Frame, error) {
	d.mu.Lock()
	opens := d.opens
	d.mu.Unlock()
	if opens == 1 {
		if d.idle {
			if d.expected {
				d.mu.Lock()
				first := d.reads == 0
				if first {
					d.reads++
					close(d.entered)
				}
				w, h := d.openW, d.openH
				d.mu.Unlock()
				if first {
					return &capture.Frame{Format: capture.FormatRaw, Width: w, Height: h, Data: []byte{0, 0, 0, 255}}, nil
				}
			}
			return nil, nil
		}
		if d.failed != nil {
			select {
			case <-d.failed:
			default:
				close(d.failed)
			}
		}
		return nil, capture.ErrStreamStopped
	}
	d.mu.Lock()
	first := d.reads == 0
	d.mu.Unlock()
	if first {
		d.mu.Lock()
		d.reads++
		close(d.entered)
		w, h := d.openW, d.openH
		d.mu.Unlock()
		return &capture.Frame{Format: capture.FormatRaw, Width: w, Height: h, Data: []byte{0, 0, 0, 255}}, nil
	}
	return d.blockingCapture.Capture()
}

func TestCaptureRecoversWithoutReconnect(t *testing.T) {
	for _, tc := range []struct {
		name     string
		idle     bool
		failures int
	}{
		{"stopped and reopen fails", false, 1},
		{"abnormal idle", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev := &recoveringCapture{idle: tc.idle, failures: tc.failures}
			b := NewBridge([]*MachineSession{{Capture: dev, CapW: 800, CapH: 600, FPS: 25}}, "alt", 30, 0, 0)
			sink := &nativeResizeSink{frames: make(chan Frame, 8)}
			b.SetRDP(sink)
			if err := b.Start(); err != nil {
				t.Fatal(err)
			}
			defer b.Stop()
			b.StartRDPWithSize("planar", 800, 600)
			select {
			case f := <-sink.frames:
				if f.Width != 800 || f.Height != 600 {
					t.Fatalf("lost geometry: %+v", f)
				}
			case <-time.After(9 * time.Second):
				t.Fatal("capture did not recover")
			}
			b.mu.Lock()
			active := b.capturing && b.consumers == 1
			b.mu.Unlock()
			dev.mu.Lock()
			fps, opens := dev.openFPS, dev.opens
			dev.mu.Unlock()
			if !active || fps != 25 || opens != 2 {
				t.Fatalf("active=%v fps=%d opens=%d", active, fps, opens)
			}
			if err := b.ResizeRDPDesktop(1024, 768); err != nil {
				t.Fatalf("lost RDP ownership: %v", err)
			}
		})
	}
}

func TestCaptureRecoveryCancelledByDisconnect(t *testing.T) {
	dev := &recoveringCapture{failed: make(chan struct{})}
	b := NewBridge([]*MachineSession{{Capture: dev, CapW: 800, CapH: 600}}, "alt", 30, 0, 0)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	b.StartRDPWithCodec("planar")
	select {
	case <-dev.failed:
	case <-time.After(time.Second):
		t.Fatal("capture did not fail")
	}
	start := time.Now()
	b.OnClientDisconnected()
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("disconnect waited for recovery backoff")
	}
	dev.mu.Lock()
	opens := dev.opens
	dev.mu.Unlock()
	if opens != 1 {
		t.Fatalf("disconnect reopened capture: %d", opens)
	}
}

func TestStaticCaptureDoesNotRestart(t *testing.T) {
	dev := &recoveringCapture{idle: true, expected: true}
	b := NewBridge([]*MachineSession{{Capture: dev, CapW: 800, CapH: 600}}, "alt", 30, 0, 0)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	b.StartRDPWithCodec("planar")
	dev.waitForRead(t)
	time.Sleep(6500 * time.Millisecond)
	dev.mu.Lock()
	opens := dev.opens
	dev.mu.Unlock()
	if opens != 1 {
		t.Fatalf("static capture reopened %d times", opens)
	}
}

func TestCaptureRecoveryBackoff(t *testing.T) {
	r := captureRecovery{}
	for _, want := range []time.Duration{1, 2, 4, 8, 16, 30, 30} {
		if got := r.retryDelay(); got != want*time.Second {
			t.Fatalf("delay=%s want=%ds", got, want)
		}
	}
	now := time.Now()
	r.frame(now)
	r.frame(now.Add(time.Second))
	if got := r.retryDelay(); got != 30*time.Second {
		t.Fatalf("brief recovery reset backoff: %s", got)
	}
	r.frame(now)
	r.frame(now.Add(30 * time.Second))
	if got := r.retryDelay(); got != time.Second {
		t.Fatalf("healthy capture did not reset backoff: %s", got)
	}
}

func TestCaptureStopDuringRecoveryOpen(t *testing.T) {
	dev := &recoveringCapture{openStarted: make(chan struct{}), openRelease: make(chan struct{})}
	b := NewBridge([]*MachineSession{{Capture: dev, CapW: 800, CapH: 600}}, "alt", 30, 0, 0)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	b.StartRDPWithCodec("planar")
	select {
	case <-dev.openStarted:
	case <-time.After(3 * time.Second):
		close(dev.openRelease)
		b.Stop()
		t.Fatal("reopen did not start")
	}
	b.mu.Lock()
	run := b.run
	b.mu.Unlock()
	done := make(chan struct{})
	go func() { b.Stop(); close(done) }()
	select {
	case <-run.stop:
	case <-time.After(time.Second):
		close(dev.openRelease)
		<-done
		t.Fatal("stop did not cancel run")
	}
	close(dev.openRelease)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stop deadlocked with recovery")
	}
	dev.mu.Lock()
	stop := dev.stop
	dev.mu.Unlock()
	select {
	case <-stop:
	default:
		t.Fatal("recovery leaked an open capture after stop")
	}
}

func TestCaptureRecoveryIdleChecks(t *testing.T) {
	now := time.Now()
	m := captureIdleMonitor{lastFrame: now}
	if m.recoveryDue(now.Add(4 * time.Second)) {
		t.Fatal("normal gap triggered recovery check")
	}
	if !m.recoveryDue(now.Add(5 * time.Second)) {
		t.Fatal("stall did not trigger recovery check")
	}
	if m.recoveryDue(now.Add(5500 * time.Millisecond)) {
		t.Fatal("recovery checks not rate limited")
	}
	if !m.recoveryDue(now.Add(6 * time.Second)) {
		t.Fatal("recovery checks stopped")
	}
}
