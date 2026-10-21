package videotiming

import (
	"testing"
	"time"
)

func TestTextureProcessingSpansPartitionCallbackToSubmit(t *testing.T) {
	now := time.Now()
	trace := &Trace{Path: "texture-stages", Acquired: now,
		TextureReadyStart: now.Add(2 * time.Millisecond), TextureReadyEnd: now.Add(3 * time.Millisecond),
		TextureImported: now.Add(4 * time.Millisecond), EncodeSubmit: now.Add(6 * time.Millisecond)}
	trace.Capture()
	stats.Lock()
	defer stats.Unlock()
	w := stats.windows["capture/texture-stages"]
	defer delete(stats.windows, "capture/texture-stages")
	var sum time.Duration
	for _, name := range []string{"capture_encode_queue", "texture_ready_wait", "texture_import_cpu", "encode_submit_prepare"} {
		if w == nil || len(w.values[name]) != 1 {
			t.Fatalf("missing stage %s", name)
		}
		sum += w.values[name][0]
	}
	if sum != 6*time.Millisecond || sum != w.values["capture_to_encode_submit"][0] {
		t.Fatal("overlapping or missing processing spans")
	}
}

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

func TestAckWaitBelongsToSelectedFrame(t *testing.T) {
	start := time.Now()
	d := Delivery{Enqueued: start.Add(90 * time.Millisecond)}
	if got := d.Waited(start, start.Add(100*time.Millisecond)); got != 10*time.Millisecond {
		t.Fatalf("fresh capture charged with an older frame's ACK wait: %s", got)
	}
	if got := d.Waited(start, start.Add(80*time.Millisecond)); got != 0 {
		t.Fatalf("negative ACK duration: %s", got)
	}
}

func TestRDPSentStagesKeepFramingAndAckSeparate(t *testing.T) {
	now := time.Now()
	trace := &Trace{Path: "diagnostic-fixture", Present: now.Add(-20 * time.Millisecond), Acquired: now.Add(-15 * time.Millisecond), EncodeSubmit: now.Add(-12 * time.Millisecond), EncodeComplete: now.Add(-8 * time.Millisecond), Ready: now.Add(-6 * time.Millisecond)}
	trace.Capture()
	d := Delivery{Trace: trace, Enqueued: now.Add(-5 * time.Millisecond)}
	d.SentRDP("rdp-gfx", now.Add(-2*time.Millisecond), time.Millisecond, 2*time.Millisecond, time.Millisecond)
	stats.Lock()
	defer stats.Unlock()
	defer delete(stats.windows, "capture/diagnostic-fixture")
	defer delete(stats.windows, "send/rdp-gfx/diagnostic-fixture")
	w := stats.windows["send/rdp-gfx/diagnostic-fixture"]
	if trace.ID == 0 || w == nil || w.values["send_call"][0] != 2*time.Millisecond || w.values["ack_wait"][0] != time.Millisecond || w.values["rdp_prepare_encode"][0] != time.Millisecond {
		t.Fatal("RDP timing combined encoding, framing/writes or ACK wait")
	}
}

func TestFutureSourcePTSIsNotAcceptedAfterEncodingCatchesUp(t *testing.T) {
	acquired := time.Now().Add(-time.Second)
	trace := &Trace{Path: "future-source-fixture", SourceClock: "test-sync-clock",
		Acquired: acquired, Present: acquired.Add(10 * time.Millisecond),
		Display: acquired.Add(15 * time.Millisecond), Ready: acquired.Add(20 * time.Millisecond)}
	trace.Capture()
	d := Delivery{Trace: trace, Enqueued: trace.Ready.Add(time.Millisecond)}
	d.SentRDP("rdp-gfx", d.Enqueued.Add(time.Millisecond), 0, 0, 0)
	c := trace.sourceClockSample()
	if c.Status != "future" || c.PTSToAcquireMS == nil || *c.PTSToAcquireMS != -10 || c.DisplayToAcquireMS == nil || *c.DisplayToAcquireMS != -15 {
		t.Fatalf("lost signed source clock diagnostics: %+v", c)
	}
	stats.Lock()
	defer stats.Unlock()
	for _, label := range []string{"capture/future-source-fixture", "send/rdp-gfx/future-source-fixture"} {
		w := stats.windows[label]
		delete(stats.windows, label)
		if w == nil {
			t.Fatalf("missing processing sample: %s", label)
		}
		for _, metric := range []string{"present_to_acquire", "present_to_ready", "present_to_socket"} {
			if len(w.values[metric]) != 0 {
				t.Fatalf("future PTS became a positive cumulative latency: %s/%s", label, metric)
			}
		}
	}
	if trace.presentationValid() {
		t.Fatal("future source PTS accepted")
	}
	trace.Present = time.Time{}
	if trace.sourceClockSample().Status != "missing" {
		t.Fatal("missing source timestamp was hidden")
	}
}
