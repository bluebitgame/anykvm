package capture

import "time"

// Pace raw source frames against fixed time slots, rather than restarting a
// full interval after every encoder submission. Half-slot tolerance chooses
// the nearest source picture and absorbs capture/polling jitter without drift.
type framePacer struct {
	next, last time.Time
}

func (p *framePacer) due(at time.Time, interval time.Duration) bool {
	return p.next.IsZero() || at.Before(p.last) || !at.Before(p.next.Add(-interval/2))
}

// Commit only after the encoder accepts a picture. Forced IDRs between slots
// do not postpone the next ordinary picture. Idle time never creates a backlog.
func (p *framePacer) accepted(at time.Time, interval time.Duration) {
	if p.next.IsZero() || at.Before(p.last) {
		p.next = at.Add(interval)
	} else if !at.Before(p.next.Add(-interval / 2)) {
		steps := (at.Sub(p.next)+interval/2)/interval + 1
		p.next = p.next.Add(steps * interval)
	}
	p.last = at
}
