package capture

import (
	"testing"
	"time"
)

func TestFramePacerJitterIdleAndForcedIDR(t *testing.T) {
	start := time.Now()
	interval := time.Second / 30
	var p framePacer
	for n := 0; n < 300; n++ {
		at := start.Add(time.Duration(n)*interval + time.Duration(n%3-1)*8*time.Millisecond)
		if !p.due(at, interval) {
			t.Fatalf("dropped jittered 30fps picture %d", n)
		}
		p.accepted(at, interval)
	}
	// A forced recovery picture between slots must not move the next slot.
	p = framePacer{}
	p.accepted(start, interval)
	next := p.next
	p.accepted(next.Add(-interval*3/4), interval)
	if p.next != next {
		t.Fatal("forced IDR delayed normal cadence")
	}
	// Long idle gaps skip expired slots without permitting a burst of catch-up.
	at := start.Add(time.Minute)
	if !p.due(at, interval) {
		t.Fatal("did not resume after idle")
	}
	p.accepted(at, interval)
	if p.due(at, interval) {
		t.Fatal("admitted duplicate after idle")
	}
	// Source clock reset/reopen must not leave input blocked by an old deadline.
	if !p.due(start, interval) {
		t.Fatal("source time reset blocked input")
	}
	p.accepted(start, interval)
	if !p.due(start.Add(interval), interval) {
		t.Fatal("reset cadence did not recover")
	}
}
