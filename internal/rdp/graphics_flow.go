package rdp

import (
	"fmt"
	"time"
)

const (
	graphicsFrameWindowMin = 2
	graphicsFrameWindowMax = 32
	graphicsQueueBudget    = 50 * time.Millisecond
	graphicsByteBudget     = 16 << 20
)

type graphicsPendingFrame struct {
	ready time.Time
	bytes uint64
	probe bool // the flight was empty when this frame was written
}

// All durations use the host monotonic clock, independently of START_FRAME's
// UTC timestamp. The minimum RTT is a propagation/decoding baseline; growing
// queueing delay must not recursively inflate the permitted flight size.
type graphicsFlowControl struct {
	interval, rtt, variation, baseline time.Duration
	frameBytes                         uint64
	queueDepth                         uint32 // MS-RDPEGFX reports bytes, not frames; zero means unknown.
	pressure                           int
	recoverUntil                       time.Time
	ackEpoch                           time.Time
	epochFrames                        int
	servicePeriod                      time.Duration
	learnService                       bool
	lastACK                            time.Time
	batchFirst, batchLast              time.Time
	batchMin, batchMax                 time.Duration
	batchProbe                         bool
	batchDelay                         time.Duration // ACK reporting delay, not evidence of a decoder queue
	batchSpan, batchCadence            time.Duration
	previousBatch                      time.Time
	batchCount, singleBatches          int
	windowLimit, windowCandidate       int
	windowCandidateAt                  time.Time
}

func (f *graphicsFlowControl) reset() {
	*f = graphicsFlowControl{interval: f.interval}
}

func (f *graphicsFlowControl) period() time.Duration {
	if f.interval > 0 {
		return max(f.interval, f.servicePeriod)
	}
	return max(time.Second/30, f.servicePeriod)
}

func (f *graphicsFlowControl) queueBudget() time.Duration {
	return max(graphicsQueueBudget, f.period())
}

func graphicsFlightFrames(d, period time.Duration) int {
	// Clamp before converting to int, including on 32-bit builds.
	return int(max(1, min(time.Duration(graphicsFrameWindowMax), (d+period-1)/period)))
}

func (f *graphicsFlowControl) queueFull() bool {
	if f.queueDepth == 0 || f.frameBytes == 0 {
		return false
	}
	frames := max(2, graphicsFlightFrames(f.queueBudget(), f.period()))
	// Queue depth describes the client when the ACK was generated. Batched
	// feedback can already be one reporting interval old on arrival; discount
	// that interval rather than repeatedly draining on stale buffered bytes.
	if f.batchDelay > 0 {
		frames += int((f.batchDelay + f.period() - 1) / f.period())
	}
	budget := min(uint64(graphicsByteBudget), max(uint64(64<<10), f.frameBytes*uint64(frames)))
	return uint64(f.queueDepth) > budget
}

func (f *graphicsFlowControl) window(now time.Time) int {
	f.flushACKBatch(now)
	if f.rtt == 0 {
		return graphicsFrameWindowMin
	}
	period := f.period()
	// Round the measured flight up to whole frames; do not add another slot
	// at exact decoder-period boundaries, which would manufacture queueing.
	margin := min(2*f.variation, f.queueBudget())
	if f.servicePeriod > f.interval+f.interval/4 && f.interval > 0 {
		// Once a slower consumer is observed, variations caused by our own
		// queue must not keep opening another decoder queue slot.
		margin = min(margin, period/4)
	}
	// A burst may acknowledge frames already decoded/rendered long ago. Account
	// for that reporting cadence separately from the decoder queue allowance.
	margin = max(margin, min(f.batchDelay, 250*time.Millisecond))
	window := graphicsFlightFrames(f.baseline+margin, period)
	ceiling := graphicsFlightFrames(f.baseline+f.queueBudget()+min(f.batchDelay, 250*time.Millisecond), period)
	window = max(graphicsFrameWindowMin, min(window, ceiling, graphicsFrameWindowMax))
	if now.Before(f.recoverUntil) || f.queueDepth >= graphicsByteBudget {
		// A slow decoder cannot be distinguished from a growing network queue
		// using RTT alone. Drain with one probe before learning a new flight.
		window = 1
	}
	if window == 1 || f.windowLimit == 0 || f.windowLimit == 1 || window > f.windowLimit+1 {
		f.windowLimit, f.windowCandidate = window, 0
	} else if window == f.windowLimit {
		f.windowCandidate = 0
	} else if window == f.windowLimit-1 {
		// A one-slot dead band prevents alternating ACK batch sizes from
		// continuously opening/closing the same slot. Actual pressure, byte
		// limits and stalled-frame admission still contract immediately.
		f.windowCandidate = 0
	} else if window != f.windowCandidate {
		f.windowCandidate, f.windowCandidateAt = window, now
	} else if now.Sub(f.windowCandidateAt) >= 500*time.Millisecond {
		f.windowLimit, f.windowCandidate = window, 0
	}
	return f.windowLimit
}

func (f *graphicsFlowControl) idleProbe(probe bool, now time.Time) bool {
	return probe && !f.lastACK.IsZero() && now.Sub(f.lastACK) > max(time.Second, 4*f.rtt, 4*f.period())
}

func (f *graphicsFlowControl) ack(sample time.Duration, probe bool, now time.Time) {
	if sample <= 0 {
		return
	}
	if f.idleProbe(probe, now) {
		// An idle desktop is not evidence of a slow decoder. Start a fresh
		// throughput measurement instead of feeding the idle gap into it.
		f.ackEpoch, f.epochFrames = now, 0
		f.servicePeriod, f.learnService, f.pressure = 0, false, 0
		f.recoverUntil = time.Time{}
		f.rtt, f.variation = 0, 0
		f.windowLimit, f.windowCandidate = 0, 0
	}
	f.lastACK = now
	// A drained flight supplies a new baseline after a real network latency
	// change; otherwise only lower samples may move it, avoiding queue feedback.
	if f.baseline == 0 || sample < f.baseline || probe {
		f.baseline = sample
	}
	if f.rtt == 0 {
		f.rtt = sample
	} else {
		difference := sample - f.rtt
		if difference < 0 {
			difference = -difference
		}
		f.variation += (difference - f.variation) / 4
		f.rtt += (sample - f.rtt) / 8
	}
	if f.rtt > f.baseline+f.queueBudget() || f.queueFull() {
		f.learnService = true
	}
	if f.rtt > f.baseline+f.queueBudget()+f.batchDelay || f.queueFull() {
		f.learnService = true
		f.pressure++
		if f.pressure >= 3 {
			f.recoverUntil = now.Add(500 * time.Millisecond)
		}
	} else {
		f.pressure = 0
	}
}

// One network read may deliver many ACK PDUs. Sample that burst once, using
// its freshest RTT and its send-time spread as jitter, rather than weighting
// the same network delay once per frame and triggering multiple drain events.
func (f *graphicsFlowControl) observeACK(sample time.Duration, probe bool, now time.Time) {
	if f.ackEpoch.IsZero() {
		f.ackEpoch = now
	} else {
		f.epochFrames++
	}
	if sample <= 0 {
		return
	}
	if f.batchFirst.IsZero() {
		f.batchFirst, f.batchMin, f.batchMax, f.batchProbe = now, sample, sample, probe
	} else {
		if sample < f.batchMin {
			f.batchMin, f.batchProbe = sample, probe
		}
		f.batchMax = max(f.batchMax, sample)
	}
	f.batchLast = now
	f.batchCount++
}

func (f *graphicsFlowControl) flushACKBatch(now time.Time) {
	if f.batchFirst.IsZero() || now.Sub(f.batchLast) < 2*time.Millisecond &&
		now.Sub(f.batchFirst) < min(16*time.Millisecond, max(2*time.Millisecond, f.period())) {
		return
	}
	if f.idleProbe(f.batchProbe, f.batchLast) {
		// A desktop idle gap is not the client's ACK reporting interval.
		f.previousBatch = time.Time{}
		f.batchSpan, f.batchCadence, f.singleBatches = 0, 0, 0
	}
	f.batchSpan += ((f.batchMax - f.batchMin) - f.batchSpan) / 4
	if !f.previousBatch.IsZero() {
		gap := f.batchLast.Sub(f.previousBatch)
		if gap > 0 && f.batchCount > 1 {
			f.singleBatches = 0
			if f.batchCadence == 0 {
				f.batchCadence = gap
			} else {
				f.batchCadence += (gap - f.batchCadence) / 8
			}
		} else if gap > 0 {
			// Several consecutive single-frame reports indicate batching
			// has ended. Their gap can be a slow decoder's service time,
			// so comparing it with half the old cadence would retain
			// artificial flight allowance and decoder backlog forever.
			f.singleBatches = min(8, f.singleBatches+1)
			if f.singleBatches >= 8 {
				f.batchCadence -= f.batchCadence / 8
				if f.batchCadence < 2*time.Millisecond {
					f.batchCadence = 0
				}
			}
		}
	}
	f.previousBatch = f.batchLast
	f.batchDelay = f.batchSpan
	if f.batchCadence > 0 {
		f.batchDelay = f.batchCadence
	}
	f.batchDelay = min(250*time.Millisecond, f.batchDelay)
	f.ack(f.batchMin, f.batchProbe, f.batchLast)
	f.variation = max(f.variation, (f.batchMax-f.batchMin)/2)
	f.updateACKRate(f.batchLast)
	f.batchFirst, f.batchLast = time.Time{}, time.Time{}
	f.batchCount = 0
}

func (f *graphicsFlowControl) updateACKRate(now time.Time) {
	// Learn decoder/transport throughput only after actual queue pressure. Doing
	// this during startup would mistake the initial small window for capacity.
	// Aggregate ACKs over 200ms so batched ACK arrival is not treated as infinite
	// decoding speed. Continued sampling lets a recovered client ramp up again.
	if !f.ackEpoch.IsZero() && f.epochFrames > 0 {
		if elapsed := now.Sub(f.ackEpoch); elapsed >= 200*time.Millisecond {
			if f.learnService {
				period := elapsed / time.Duration(f.epochFrames)
				if f.servicePeriod == 0 {
					f.servicePeriod = period
				} else {
					f.servicePeriod += (period - f.servicePeriod) / 4
				}
				if !now.Before(f.recoverUntil) && !f.queueFull() && f.rtt <= f.baseline+f.queueBudget()/2 {
					// Probe available capacity when queues have drained. Otherwise
					// the throughput of our own reduced window becomes a permanent
					// rate cap even after the client/network has recovered.
					// Do not add reporting delay here: it already grants flight
					// slots, and would mistake a stable decoder queue for room
					// to probe faster on every measurement epoch.
					source := f.interval
					if source == 0 {
						source = time.Second / 30
					}
					if f.servicePeriod > source {
						f.servicePeriod -= min(time.Millisecond, f.servicePeriod-source)
					}
				}
			}
			f.ackEpoch, f.epochFrames = now, 0
		}
	}
}

func (c *client) setGraphicsFPS(fps int) {
	if fps < 1 || fps > 240 {
		fps = 30
	}
	c.ackMu.Lock()
	c.graphicsFlow.interval = time.Second / time.Duration(fps)
	c.ackMu.Unlock()
}

// Register bytes before each write, and start RTT sampling immediately before
// the batch containing END_FRAME. An ACK can arrive before sendGFX returns.
func (c *client) recordGraphicsWrite(id uint32, size int, final bool, now time.Time) {
	c.ackMu.Lock()
	defer c.ackMu.Unlock()
	pending, ok := c.pendingFrames[id]
	if !ok {
		return
	}
	pending.bytes += uint64(size)
	if final {
		pending.ready = now
		pending.probe = len(c.pendingFrames) == 1
		if c.graphicsFlow.frameBytes == 0 {
			c.graphicsFlow.frameBytes = pending.bytes
		} else {
			c.graphicsFlow.frameBytes = (7*c.graphicsFlow.frameBytes + pending.bytes) / 8
		}
	}
	c.pendingFrames[id] = pending
}

// Caller holds ackMu. Refinements reserve one flight slot for fresh captures.
func (c *client) graphicsWindowFull(now time.Time, refinement bool) bool {
	return c.graphicsWindowFullExcept(now, refinement, nil)
}

// A prepared but unwritten background frame must recheck admission without
// counting its own reservation. ACK feedback can shrink the window meanwhile.
func (c *client) graphicsWindowFullExcept(now time.Time, refinement bool, reserved *uint32) bool {
	if c.suspendAck {
		return false
	}
	limit := c.graphicsFlow.window(now)
	if refinement {
		if limit <= 1 {
			return true // congestion probes belong to fresh captures
		}
		limit--
	}
	count := len(c.pendingFrames)
	if reserved != nil {
		if _, ok := c.pendingFrames[*reserved]; ok {
			count--
		}
	}
	if count == 0 {
		return false
	}
	if count >= limit {
		return true
	}
	var bytes uint64
	for id, pending := range c.pendingFrames {
		if reserved != nil && id == *reserved {
			continue
		}
		bytes += pending.bytes
		if c.graphicsFlow.baseline > 0 && !pending.ready.IsZero() &&
			now.Sub(pending.ready) > c.graphicsFlow.baseline+2*c.graphicsFlow.queueBudget()+min(c.graphicsFlow.batchDelay, 250*time.Millisecond) {
			return true
		}
	}
	budget := uint64(graphicsByteBudget)
	if refinement {
		// Leave byte headroom as well as a frame slot for a fresh capture.
		budget -= min(budget, max(uint64(256<<10), c.graphicsFlow.frameBytes))
	}
	return bytes >= budget
}

func (c *client) refinementWindowFull() bool {
	c.ackMu.Lock()
	defer c.ackMu.Unlock()
	return c.graphicsWindowFull(time.Now(), true)
}

func (c *client) graphicsFlowStatus() string {
	c.ackMu.Lock()
	defer c.ackMu.Unlock()
	var bytes uint64
	for _, pending := range c.pendingFrames {
		bytes += pending.bytes
	}
	return fmt.Sprintf("pending=%d/%d pending_kib=%d ack_rtt_ms=%.1f ack_base_ms=%.1f ack_jitter_ms=%.1f ack_batch_ms=%.1f flow_fps=%.1f client_queue_kib=%d ack_suspended=%t last_ack=%d",
		len(c.pendingFrames), c.graphicsFlow.window(time.Now()), bytes/1024,
		float64(c.graphicsFlow.rtt)/float64(time.Millisecond), float64(c.graphicsFlow.baseline)/float64(time.Millisecond),
		float64(c.graphicsFlow.variation)/float64(time.Millisecond), float64(c.graphicsFlow.batchDelay)/float64(time.Millisecond), float64(time.Second)/float64(c.graphicsFlow.period()),
		c.graphicsFlow.queueDepth/1024, c.suspendAck, c.acknowledged)
}
