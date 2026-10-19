package bridge

import (
	"testing"
	"time"
)

func TestCaptureIdleDiagnostics(t *testing.T) {
	start := time.Now()
	m := captureIdleMonitor{lastFrame: start}
	for _, gap := range []time.Duration{100 * time.Millisecond, time.Second, 4999 * time.Millisecond} {
		if m.due(start.Add(gap)) {
			t.Fatalf("normal frame gap %s triggered stall diagnostic", gap)
		}
	}
	if !m.due(start.Add(5 * time.Second)) {
		t.Fatal("missing prolonged/first-frame diagnostic")
	}
	m.lastCheck = start.Add(5 * time.Second)
	m.lastFrame = start.Add(6 * time.Second) // a brief recovery must not reset rate limit
	if m.due(start.Add(11 * time.Second)) {
		t.Fatal("intermittent source flooded diagnostics")
	}
	if !m.due(start.Add(35 * time.Second)) {
		t.Fatal("persistent stall stopped being checked")
	}
	m.lastFrame = start.Add(35 * time.Second)
	if m.due(start.Add(36 * time.Second)) {
		t.Fatal("recovered source still reported idle")
	}
}
