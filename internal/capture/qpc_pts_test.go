package capture

import "testing"

func TestQPCPresentationPTSPreservesIntervalsAndOrigin(t *testing.T) {
	const frequency int64 = 10_000_000
	// Long uptime, with exact microseconds and sub-microsecond fractions.
	const origin int64 = 7_000_000_000_000_000
	for i, delta := range []int64{0, 166670, 333330, 500009} {
		got := qpcPresentationPTS(origin+delta, origin+delta+50000, frequency, false)
		if want := origin/10 + delta/10; got != want {
			t.Fatalf("picture %d: PTS=%d want=%d", i, got, want)
		}
	}
	// Reopening capture or selecting another codec has no state to reset.
	first := qpcPresentationPTS(origin, origin+10000, frequency, false)
	second := qpcPresentationPTS(origin+10000000, origin+10010000, frequency, false)
	if second-first != 1_000_000 {
		t.Fatal("capture reopen changed the QPC origin")
	}
}

func TestQPCPresentationPTSRecoveryAndMissingPresentUseAcquiredQPC(t *testing.T) {
	for _, tc := range []struct {
		present, acquired int64
		repeated          bool
		want              int64
	}{
		{10_000_000, 50_000_000, false, 1_000_000},
		{10_000_000, 50_000_000, true, 5_000_000},
		{0, 50_000_000, false, 5_000_000},
		{60_000_000, 50_000_000, false, 5_000_000},
	} {
		if got := qpcPresentationPTS(tc.present, tc.acquired, 10_000_000, tc.repeated); got != tc.want {
			t.Fatalf("%+v: PTS=%d", tc, got)
		}
	}
}

func TestQPCPresentationPTSRejectsInvalidAndUnsafeValues(t *testing.T) {
	for _, tc := range []struct{ ticks, frequency int64 }{
		{0, 10_000_000}, {10, 0}, {10, -1}, {-1, 10_000_000},
		{1<<63 - 1, 1}, {1<<63 - 1, 1_000_000_000},
	} {
		if got := qpcPresentationPTS(tc.ticks, tc.ticks, tc.frequency, false); got != 0 {
			t.Fatalf("%+v: unsafe PTS=%d", tc, got)
		}
	}
	// The fractional multiply must not overflow at an unusually high frequency.
	if got := qpcPresentationPTS(1<<62, 1<<62, 1<<62+1, false); got != 999999 {
		t.Fatalf("fractional conversion overflow: %d", got)
	}
}
