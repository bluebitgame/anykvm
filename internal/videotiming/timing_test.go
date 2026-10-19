package videotiming

import (
	"testing"
	"time"
)

func TestQPCMappingPreservesOldPresentationAndLongUptime(t *testing.T) {
	anchor := time.Now()
	const frequency int64 = 10_000_000
	const uptime int64 = 100_000_000_000_000_000
	for _, offset := range []time.Duration{-48 * time.Hour, -17 * time.Millisecond, 0, 3 * time.Millisecond} {
		ticks := uptime + int64(offset/time.Second)*frequency + int64(offset%time.Second)*frequency/int64(time.Second)
		got := QPCTime(ticks, frequency, uptime, anchor)
		if got.Sub(anchor) != offset {
			t.Fatalf("offset %v mapped to %v", offset, got.Sub(anchor))
		}
	}
	for _, tc := range []struct{ ticks, freq, now int64 }{{0, frequency, uptime}, {uptime, 0, uptime}, {uptime, frequency, 0}, {1, frequency, uptime}} {
		if got := QPCTime(tc.ticks, tc.freq, tc.now, anchor); !got.IsZero() {
			t.Fatalf("invalid clock mapped to %v", got)
		}
	}
}

func TestMissingAndRepeatedStagesDoNotInventDurations(t *testing.T) {
	now := time.Now()
	v := make(map[string]time.Duration)
	add(v, "missing", time.Time{}, now)
	add(v, "backwards", now, now.Add(-time.Second))
	if len(v) != 0 {
		t.Fatalf("invented durations: %v", v)
	}
	trace := &Trace{Anchor: now, AnchorQPC: 1000, Frequency: 1000}
	trace.Native(Native{EncodeSubmit: 998, EncodeComplete: 1000})
	if !trace.ConversionSubmitted.IsZero() || trace.EncodeComplete.Sub(trace.EncodeSubmit) != 2*time.Millisecond {
		t.Fatalf("wrong native mapping: %+v", trace)
	}
	// Consumer admission must not mutate the source or another consumer's clock.
	original := Delivery{Trace: trace}
	a, b := original.Enqueue(), original.Enqueue()
	if !original.Enqueued.IsZero() || a.Enqueued.IsZero() || b.Enqueued.IsZero() || a.Trace != b.Trace {
		t.Fatal("delivery timing did not preserve independent consumer metadata")
	}
	trace.Path, trace.Repeated = "test-repeated", true
	trace.Present, trace.Acquired, trace.Ready = now.Add(-time.Hour), now, now
	trace.Capture()
	stats.Lock()
	defer stats.Unlock()
	w := stats.windows["capture/test-repeated"]
	delete(stats.windows, "capture/test-repeated")
	if w == nil || len(w.values["present_to_ready"]) != 0 || len(w.values["present_to_acquire"]) != 0 || len(w.values["acquire_to_ready"]) != 1 {
		t.Fatal("static IDR polluted presentation age statistics or lost processing statistics")
	}
}

func TestWindowQuantilesUseNearestRank(t *testing.T) {
	v := []time.Duration{time.Millisecond, 2 * time.Millisecond, 20 * time.Millisecond}
	if quantile(v, 50) != 2*time.Millisecond || quantile(v, 95) != 20*time.Millisecond || quantile(v, 99) != 20*time.Millisecond {
		t.Fatal("tail percentiles hide the slow sample")
	}
}

func TestStaticRecoveryDoesNotReuseOldPlaybackPTS(t *testing.T) {
	now := time.Now()
	present := now.Add(-time.Hour)
	if !PictureTime(present, now, false).Equal(present) {
		t.Fatal("new picture lost its original presentation time")
	}
	if !PictureTime(present, now, true).Equal(now) {
		t.Fatal("static recovery IDR reused an old decoder timestamp")
	}
	if !PictureTime(time.Time{}, now, false).Equal(now) || !PictureTime(now.Add(time.Second), now, false).Equal(now) {
		t.Fatal("invalid presentation time was not replaced by acquisition time")
	}
}
