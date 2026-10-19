package bridge

import "time"

// Poll counts are not elapsed time: event-driven capture can legitimately be
// idle between updates. Check prolonged gaps and rate-limit diagnostics across
// brief recoveries so an intermittent source cannot flood the log.
type captureIdleMonitor struct {
	lastFrame         time.Time
	lastCheck         time.Time
	lastRecoveryCheck time.Time
}

func (m *captureIdleMonitor) recoveryDue(now time.Time) bool {
	if now.Sub(m.lastFrame) < 5*time.Second || (!m.lastRecoveryCheck.IsZero() && now.Sub(m.lastRecoveryCheck) < time.Second) {
		return false
	}
	m.lastRecoveryCheck = now
	return true
}

func (m captureIdleMonitor) due(now time.Time) bool {
	return now.Sub(m.lastFrame) >= 5*time.Second &&
		(m.lastCheck.IsZero() || now.Sub(m.lastCheck) >= 30*time.Second)
}
