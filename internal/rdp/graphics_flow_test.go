package rdp

import (
	"sort"
	"testing"
	"time"
)

func TestGraphicsFlowRTTAndJitterWindow(t *testing.T) {
	now := time.Unix(100, 0)
	f := graphicsFlowControl{interval: time.Second / 60}
	if f.window(now) != 2 {
		t.Fatal("startup must begin with a bounded flight")
	}
	f.ack(50*time.Millisecond, false, now)
	if n := f.window(now); n < 4 || n > 5 {
		t.Fatalf("50ms RTT at 60fps needs more than two slots, got %d", n)
	}
	for i, sample := range []time.Duration{70, 55, 80, 60, 75, 50} {
		f.ack(sample*time.Millisecond, false, now.Add(time.Duration(i+1)*time.Second/60))
	}
	if n := f.window(now); n < 4 || n > 7 {
		t.Fatalf("jitter allowance exceeded its latency budget: %d", n)
	}
	f.interval = time.Second / 240
	f.baseline = time.Second
	if n := f.window(now); n != graphicsFrameWindowMax {
		t.Fatalf("flight exceeded its hard frame cap: %d", n)
	}
}

func TestGraphicsFlowQueuePressureAndRecovery(t *testing.T) {
	now := time.Unix(100, 0)
	f := graphicsFlowControl{interval: time.Second / 60}
	f.ack(40*time.Millisecond, true, now)
	for i := range 20 {
		f.ack(180*time.Millisecond, false, now.Add(time.Duration(i+1)*time.Second/60))
	}
	if f.window(now.Add(350*time.Millisecond)) != 1 || !f.learnService {
		t.Fatal("sustained queueing did not enter drain mode")
	}
	if f.baseline != 40*time.Millisecond {
		t.Fatal("queueing delay inflated the propagation baseline")
	}
	// A new isolated send, not the last ACK of an old queued flight, can
	// establish a baseline after a real route-latency change.
	f.ack(100*time.Millisecond, true, now.Add(time.Second))
	if f.baseline != 100*time.Millisecond {
		t.Fatal("a drained probe did not relearn network latency")
	}
	f.frameBytes, f.queueDepth = 64<<10, 2<<20
	for i := 0; i < 3; i++ {
		f.ack(100*time.Millisecond, false, now.Add(2*time.Second+time.Duration(i)*time.Second/60))
	}
	if f.window(now.Add(2100*time.Millisecond)) != 1 {
		t.Fatal("reported client backlog did not restrict admission")
	}
	f.reset()
	if f.rtt != 0 || f.queueDepth != 0 || f.servicePeriod != 0 || f.interval != time.Second/60 {
		t.Fatal("ACK suspension retained stale estimates or lost the source FPS")
	}
}

func TestGraphicsFlowIdleDoesNotLowerCapacity(t *testing.T) {
	now := time.Unix(100, 0)
	f := graphicsFlowControl{interval: time.Second / 60, servicePeriod: time.Second / 10, learnService: true}
	f.ack(50*time.Millisecond, true, now)
	f.ack(50*time.Millisecond, true, now.Add(5*time.Second))
	if f.servicePeriod != 0 || f.learnService || f.window(now.Add(5*time.Second)) < 4 {
		t.Fatal("idle time was mistaken for decoder throughput")
	}
}

func TestGraphicsFlowIdleClearsBatchCadence(t *testing.T) {
	now := time.Unix(100, 0)
	f := graphicsFlowControl{interval: time.Second / 60}
	for batch := range 5 {
		at := now.Add(time.Duration(batch) * 100 * time.Millisecond)
		f.observeACK(80*time.Millisecond, false, at)
		f.observeACK(50*time.Millisecond, false, at)
		f.flushACKBatch(at.Add(3 * time.Millisecond))
	}
	if f.batchDelay < 90*time.Millisecond {
		t.Fatal("fixture did not learn batched ACK cadence")
	}
	at := now.Add(5 * time.Second)
	f.observeACK(50*time.Millisecond, true, at)
	f.flushACKBatch(at.Add(3 * time.Millisecond))
	if f.batchDelay != 0 || f.servicePeriod != 0 || f.window(at) > 5 {
		t.Fatal("idle probe retained stale batching/capacity estimates")
	}
}

func TestGraphicsFlowWriteAccountingAndDuplicateACK(t *testing.T) {
	c := &client{}
	now := time.Unix(100, 0)
	id := c.nextGraphicsFrame()
	c.recordGraphicsWrite(id, 100000, false, now)
	if !c.pendingFrames[id].ready.IsZero() {
		t.Fatal("partial bitmap frame became an RTT sample")
	}
	c.recordGraphicsWrite(id, 50000, true, now.Add(time.Second))
	if c.pendingFrames[id].bytes != 150000 {
		t.Fatal("fragmented frame bytes were not accumulated")
	}
	if err := c.acknowledgeFrameAt(id, 0, now.Add(time.Second+40*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	c.graphicsFlow.flushACKBatch(now.Add(time.Second + 45*time.Millisecond))
	if c.graphicsFlow.rtt != 40*time.Millisecond || len(c.pendingFrames) != 0 {
		t.Fatal("encoding/earlier writes contaminated the RTT or ACK accounting")
	}
	before := c.graphicsFlow
	if err := c.acknowledgeFrameAt(id, graphicsByteBudget, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if c.graphicsFlow != before {
		t.Fatal("duplicate ACK changed latency or queue estimates")
	}
}

func TestGraphicsFlowByteAgeAndRefinementLimits(t *testing.T) {
	now := time.Unix(100, 0)
	c := &client{graphicsFlow: graphicsFlowControl{interval: time.Second / 60, rtt: 100 * time.Millisecond, baseline: 100 * time.Millisecond}}
	for range 2 {
		id := c.nextGraphicsFrame()
		c.recordGraphicsWrite(id, graphicsByteBudget/2, true, now)
	}
	if !c.graphicsWindowFull(now, false) {
		t.Fatal("byte pressure exceeded the pending budget")
	}
	clear(c.pendingFrames)
	id := c.nextGraphicsFrame()
	c.recordGraphicsWrite(id, 1024, true, now)
	if c.graphicsWindowFull(now, false) || !c.graphicsWindowFull(now.Add(201*time.Millisecond), false) {
		t.Fatal("stalled ACK flight did not stop accepting newer frames")
	}
	limit := c.graphicsFlow.window(now)
	for len(c.pendingFrames) < limit-1 {
		c.nextGraphicsFrame()
	}
	if c.graphicsWindowFull(now, false) || !c.graphicsWindowFull(now, true) {
		t.Fatal("refinement consumed the slot reserved for fresh captures")
	}
	clear(c.pendingFrames)
	c.graphicsFlow.queueDepth = graphicsByteBudget
	if c.graphicsWindowFull(now, false) {
		t.Fatal("stale client queue feedback blocked an empty-flight probe")
	}
}

type graphicsFlowEvent struct {
	id                        uint32
	sent, arrive, decoded, at time.Time
}

type graphicsFlowScenario struct {
	batch                   time.Duration
	reverse                 bool
	pause, resume, duration time.Duration
	slowAt, batchUntil      time.Duration
	stats                   *graphicsFlowScenarioStats
}
type graphicsFlowScenarioStats struct {
	windowChanges, drains int
	lastWindow            int
}

// A virtual clock exercises source cadence, delayed/batched ACKs and a serial
// client decoder without wall-clock sleeps or real graphics/encoder hardware.
func simulateGraphicsFlow(t *testing.T, network, decode time.Duration, jitter []time.Duration, queueFeedback bool, recoverAt time.Duration, options ...graphicsFlowScenario) (fps float64, worst time.Duration, maxPending int) {
	t.Helper()
	c := &client{}
	c.setGraphicsFPS(60)
	start := time.Unix(100, 0)
	nextCapture, decoderReady := start, start
	var events []graphicsFlowEvent
	var option graphicsFlowScenario
	if len(options) > 0 {
		option = options[0]
	}
	duration := option.duration
	if duration == 0 {
		duration = 10 * time.Second
	}
	warmup := duration - 5*time.Second
	fresh, outputs, sequence := false, 0, 0
	for now := start; now.Before(start.Add(duration)); now = now.Add(time.Millisecond) {
		woke := false
		sort.SliceStable(events, func(i, j int) bool {
			if events[i].at.Equal(events[j].at) && option.reverse {
				return events[i].id > events[j].id
			}
			return events[i].at.Before(events[j].at)
		})
		for i := 0; i < len(events); {
			event := events[i]
			if event.at.After(now) {
				i++
				continue
			}
			events = append(events[:i], events[i+1:]...)
			woke = true
			var queued uint32
			if decode != 0 && queueFeedback {
				for _, other := range events {
					reportAt := event.at.Add(-network / 2)
					if !other.arrive.After(reportAt) && other.decoded.After(reportAt) {
						queued += 64 << 10
					}
				}
			}
			if option.resume > 0 && now.Sub(start) >= option.pause && now.Sub(start) < option.resume {
				queued = 0xffffffff
			}
			if err := c.acknowledgeFrameAt(event.id, queued, now); err != nil {
				t.Fatal(err)
			}
			if !event.sent.Before(start.Add(warmup)) {
				worst = max(worst, now.Sub(event.sent))
			}
		}
		if !now.Before(nextCapture) {
			woke = true
			fresh = true
			nextCapture = nextCapture.Add(time.Second / 60)
		}
		if !woke {
			continue
		}
		if option.stats != nil && now.Sub(start) >= warmup {
			n := c.graphicsFlow.window(now)
			if option.stats.lastWindow != 0 && option.stats.lastWindow != n {
				option.stats.windowChanges++
				if n == 1 {
					option.stats.drains++
				}
			}
			option.stats.lastWindow = n
		}
		if !fresh || c.graphicsWindowFull(now, false) {
			continue
		}
		id := c.nextGraphicsFrame()
		c.recordGraphicsWrite(id, 64<<10, true, now)
		arrive := now.Add(network / 2)
		at := now.Add(network)
		decodeTime := decode
		if now.Sub(start) < option.slowAt || recoverAt != 0 && now.Sub(start) >= recoverAt {
			decodeTime = 0
		}
		if decodeTime != 0 {
			decoderReady = maxTime(arrive, decoderReady).Add(decodeTime)
			at = decoderReady.Add(network / 2)
		}
		decoded := at.Add(-network / 2)
		if len(jitter) != 0 {
			at = at.Add(jitter[sequence%len(jitter)])
		}
		if option.batch > 0 && (option.batchUntil == 0 || now.Sub(start) < option.batchUntil) {
			delay := at.Sub(start)
			at = start.Add((delay + option.batch - 1) / option.batch * option.batch)
		}
		events = append(events, graphicsFlowEvent{id: id, sent: now, arrive: arrive, decoded: decoded, at: at})
		maxPending = max(maxPending, len(c.pendingFrames))
		if !now.Before(start.Add(warmup)) {
			outputs++
		}
		sequence++
		fresh = false
	}
	return float64(outputs) / 5, worst, maxPending
}

func TestGraphicsFlowBatchedAndReorderedACKs(t *testing.T) {
	for _, batch := range []time.Duration{16 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond} {
		for _, reverse := range []bool{false, true} {
			stats := &graphicsFlowScenarioStats{}
			fps, worst, pending := simulateGraphicsFlow(t, 50*time.Millisecond, 0, nil, false, 0, graphicsFlowScenario{batch: batch, reverse: reverse, duration: 20 * time.Second, stats: stats})
			t.Logf("batch=%s reverse=%t: %.1ffps delay=%s pending=%d drains=%d changes=%d", batch, reverse, fps, worst, pending, stats.drains, stats.windowChanges)
			if fps < 55 || pending > graphicsFrameWindowMax || stats.drains > 1 {
				t.Fatalf("ACK batching caused persistent throttling/oscillation")
			}
		}
	}
}

func TestGraphicsFlowPausedEpochAndStaleFeedback(t *testing.T) {
	c := &client{}
	now := time.Unix(100, 0)
	a, b := c.nextGraphicsFrame(), c.nextGraphicsFrame()
	c.recordGraphicsWrite(a, 64<<10, true, now)
	c.recordGraphicsWrite(b, 64<<10, true, now)
	if err := c.acknowledgeFrameAt(a, 0xffffffff, now.Add(50*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := c.acknowledgeFrameAt(b, 0, now.Add(51*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if !c.suspendAck || len(c.pendingFrames) != 0 {
		t.Fatal("old epoch ACK resumed a suspended session")
	}
	resume := c.nextGraphicsFrame()
	if err := c.acknowledgeFrameAt(resume, 0, now.Add(60*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if c.suspendAck {
		t.Fatal("new epoch ACK failed to resume")
	}
	if err := c.acknowledgeFrameAt(a, 0xffffffff, now.Add(61*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if c.suspendAck {
		t.Fatal("duplicate old SUSPEND reset the resumed session")
	}
	old, newer := c.nextGraphicsFrame(), c.nextGraphicsFrame()
	c.recordGraphicsWrite(old, 64<<10, true, now.Add(62*time.Millisecond))
	c.recordGraphicsWrite(newer, 64<<10, true, now.Add(63*time.Millisecond))
	if err := c.acknowledgeFrameAt(newer, 0, now.Add(100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := c.acknowledgeFrameAt(old, 16<<20, now.Add(101*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if c.graphicsFlow.queueDepth != 0 || len(c.pendingFrames) != 0 {
		t.Fatal("out-of-order ACK restored stale queue pressure")
	}
}

func TestGraphicsFlowPausedEpochAcrossRollover(t *testing.T) {
	c := &client{sent: 0xfffffffd}
	now := time.Unix(100, 0)
	a, b := c.nextGraphicsFrame(), c.nextGraphicsFrame()
	if err := c.acknowledgeFrameAt(a, 0xffffffff, now); err != nil {
		t.Fatal(err)
	}
	if err := c.acknowledgeFrameAt(b, 0, now); err != nil || !c.suspendAck {
		t.Fatalf("old epoch resumed across rollover: %v", err)
	}
	resume := c.nextGraphicsFrame()
	if resume != 0 {
		t.Fatal("fixture did not wrap the frame ID")
	}
	if err := c.acknowledgeFrameAt(resume, 0, now); err != nil || c.suspendAck {
		t.Fatalf("new epoch failed to resume across rollover: %v", err)
	}
	if err := c.acknowledgeFrameAt(a, 0xffffffff, now); err != nil || c.suspendAck {
		t.Fatalf("old SUSPEND paused the new epoch: %v", err)
	}
}

func TestGraphicsFlowBatchedPauseAndDecoderRecovery(t *testing.T) {
	for _, slow := range []bool{false, true} {
		decode, recoverAt := time.Duration(0), time.Duration(0)
		if slow {
			decode = 40 * time.Millisecond
			recoverAt = 6 * time.Second
		}
		stats := &graphicsFlowScenarioStats{}
		fps, worst, pending := simulateGraphicsFlow(t, 50*time.Millisecond, decode, nil, true, recoverAt, graphicsFlowScenario{batch: 50 * time.Millisecond, reverse: true, pause: 2 * time.Second, resume: 4 * time.Second, duration: 30 * time.Second, stats: stats})
		t.Logf("pause + batched/reversed ACKs, slow=%t: %.1ffps delay=%s pending=%d drains=%d changes=%d", slow, fps, worst, pending, stats.drains, stats.windowChanges)
		if fps < 55 || stats.drains > 1 || pending > graphicsFrameWindowMax {
			t.Fatal("pause/recovery caused persistent queueing or window oscillation")
		}
	}
}

func TestGraphicsFlowBatchedSlowDecoderStability(t *testing.T) {
	for _, batch := range []time.Duration{50 * time.Millisecond, 100 * time.Millisecond} {
		for _, feedback := range []bool{false, true} {
			stats := &graphicsFlowScenarioStats{}
			fps, worst, pending := simulateGraphicsFlow(t, 20*time.Millisecond, 40*time.Millisecond, nil, feedback, 0, graphicsFlowScenario{batch: batch, reverse: true, duration: 30 * time.Second, stats: stats})
			t.Logf("slow decoder + batch=%s feedback=%t: %.1ffps delay=%s pending=%d drains=%d changes=%d", batch, feedback, fps, worst, pending, stats.drains, stats.windowChanges)
			if fps < 19 || fps > 26 || worst > 240*time.Millisecond || pending > graphicsFrameWindowMax || stats.drains > 1 || stats.windowChanges > 4 {
				t.Fatal("batched slow-decoder feedback caused queue growth or repeated window cycling")
			}
		}
	}
}

func TestGraphicsFlowClientSlowsThenRecovers(t *testing.T) {
	for _, batch := range []time.Duration{0, 50 * time.Millisecond, 100 * time.Millisecond} {
		for _, feedback := range []bool{false, true} {
			stats := &graphicsFlowScenarioStats{}
			fps, worst, pending := simulateGraphicsFlow(t, 50*time.Millisecond, 40*time.Millisecond, nil, feedback, 10*time.Second,
				graphicsFlowScenario{slowAt: 2 * time.Second, batch: batch, reverse: true, duration: 30 * time.Second, stats: stats})
			t.Logf("slow then recovered, batch=%s feedback=%t: %.1ffps delay=%s pending=%d drains=%d changes=%d", batch, feedback, fps, worst, pending, stats.drains, stats.windowChanges)
			if fps < 57 || worst > 50*time.Millisecond+batch+35*time.Millisecond || pending > graphicsFrameWindowMax || stats.drains > 1 || stats.windowChanges > 4 {
				t.Fatal("a recovered client retained backlog or an unstable window")
			}
		}
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func TestGraphicsFlowSimulatedRTTAndJitter(t *testing.T) {
	for _, rtt := range []time.Duration{50 * time.Millisecond, 200 * time.Millisecond} {
		fps, _, pending := simulateGraphicsFlow(t, rtt, 0, []time.Duration{0, 5 * time.Millisecond, 25 * time.Millisecond, 10 * time.Millisecond, 0, 35 * time.Millisecond}, false, 0)
		t.Logf("RTT=%s with up to 35ms ACK jitter: %.1ffps, maximum %d outstanding frames", rtt, fps, pending)
		if fps < 57 || pending > graphicsFrameWindowMax {
			t.Fatalf("RTT=%s: output=%.1ffps pending=%d", rtt, fps, pending)
		}
	}
}

func TestGraphicsFlowSimulatedSlowDecoder(t *testing.T) {
	for _, feedback := range []bool{false, true} {
		fps, worst, pending := simulateGraphicsFlow(t, 20*time.Millisecond, 40*time.Millisecond, nil, feedback, 0)
		t.Logf("25fps serial decoder (queue feedback=%t): %.1ffps, steady-state worst ACK latency %s, maximum %d outstanding frames", feedback, fps, worst, pending)
		if fps < 24 || fps > 26 || worst > 120*time.Millisecond || pending > graphicsFrameWindowMax {
			t.Fatalf("25fps decoder: output=%.1ffps worst ACK latency=%s pending=%d", fps, worst, pending)
		}
	}
}

func TestGraphicsFlowSimulatedDecoderRecovery(t *testing.T) {
	fps, worst, _ := simulateGraphicsFlow(t, 50*time.Millisecond, 40*time.Millisecond, nil, true, 3*time.Second)
	t.Logf("recovered decoder: %.1ffps, steady-state worst ACK latency %s", fps, worst)
	if fps < 57 || worst > 100*time.Millisecond {
		t.Fatalf("recovered decoder remained throttled: %.1ffps, worst ACK latency %s", fps, worst)
	}
}

func TestGraphicsFlowBatchReportingEnds(t *testing.T) {
	for _, feedback := range []bool{false, true} {
		stats := &graphicsFlowScenarioStats{}
		fps, worst, pending := simulateGraphicsFlow(t, 20*time.Millisecond, 40*time.Millisecond, nil, feedback, 0, graphicsFlowScenario{batch: 100 * time.Millisecond, batchUntil: 12 * time.Second, duration: 30 * time.Second, stats: stats})
		t.Logf("batching ends, feedback=%t: %.1ffps delay=%s pending=%d drains=%d changes=%d", feedback, fps, worst, pending, stats.drains, stats.windowChanges)
		if fps < 24 || fps > 26 || worst > 120*time.Millisecond || stats.drains > 1 || stats.windowChanges > 4 {
			t.Fatal("old batch allowance retained queueing or window cycling")
		}
	}
}
