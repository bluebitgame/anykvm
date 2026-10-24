// Package videotiming records sampled host-side video latency. CPU submission
// timestamps never imply GPU completion or remote display presentation.
package videotiming

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"anykvm/internal/logging"
)

var Enabled = os.Getenv("ANYKVM_VIDEO_TIMING") == "1"
var sequence atomic.Uint64
var traceSequence atomic.Uint64
var sampleEvery = timingSampleEvery()

func timingSampleEvery() uint64 {
	n, err := strconv.ParseUint(os.Getenv("ANYKVM_VIDEO_TIMING_EVERY"), 10, 64)
	if err != nil || n < 1 || n > 10000 {
		return 30
	}
	return n
}

// Sample keeps trace allocation and aggregation off most frames. Native QPC
// markers are enabled for the session; the default is one picture in 30.
func Sample() bool { return Enabled && (sequence.Add(1)-1)%sampleEvery == 0 }

// Native contains QPC ticks, all on the host CPU clock. ConversionEnd is the
// return from GPU work submission, not completion of the conversion on the GPU.
type Native struct {
	ConversionStart, ConversionEnd int64
	EncodeSubmit, EncodeComplete   int64
}

// Trace is populated by capture, then treated as immutable by every consumer.
type Trace struct {
	ID                                                  uint64
	Present, AcquireStart, Acquired, CopySubmitted      time.Time
	Display                                             time.Time // display scheduling reference, never capture start
	SourceClock                                         string
	TextureReadyStart, TextureReadyEnd, TextureImported time.Time
	ConversionStart, ConversionSubmitted                time.Time
	EncodeSubmit, EncodeComplete, Ready                 time.Time
	Anchor                                              time.Time
	AnchorQPC, Frequency                                int64
	Path                                                string
	Repeated                                            bool
}

// QPCTime preserves the Go monotonic anchor and avoids multiplying an absolute
// uptime counter by nanoseconds (which would overflow on long-running hosts).
func QPCTime(ticks, frequency, anchorTicks int64, anchor time.Time) time.Time {
	if ticks <= 0 || frequency <= 0 || anchorTicks <= 0 {
		return time.Time{}
	}
	delta := ticks - anchorTicks
	seconds, remainder := delta/frequency, delta%frequency
	const maxSeconds = int64(time.Duration(1<<63-1) / time.Second)
	if seconds >= maxSeconds || seconds <= -maxSeconds {
		return time.Time{} // stale/invalid clock data must not corrupt playback PTS
	}
	return anchor.Add(time.Duration(seconds)*time.Second + time.Duration(float64(remainder)*float64(time.Second)/float64(frequency)))
}

// QPCClock keeps one QPC-to-Go mapping for host diagnostics in a capture session.
// It is independent of the source's QPC-relative playback PTS. Recalibrating on
// every picture would add variable scheduling delay between the native counter
// read and the Go clock read to otherwise stable source presentation intervals.
// The capture owner serializes access and resets this clock when reopening.
type QPCClock struct {
	Anchor             time.Time
	Counter, Frequency int64
	lastCounter        int64
}

func (c *QPCClock) Observe(counter, frequency int64, anchor time.Time) {
	if counter <= 0 || frequency <= 0 || anchor.IsZero() {
		return
	}
	if c.Anchor.IsZero() || c.Frequency != frequency || counter < c.lastCounter {
		c.Anchor, c.Counter, c.Frequency = anchor, counter, frequency
	}
	c.lastCounter = counter
}

func (c *QPCClock) Time(ticks int64) time.Time {
	return QPCTime(ticks, c.Frequency, c.Counter, c.Anchor)
}

// PictureTime keeps new desktop pictures on their presentation clock. A static
// recovery IDR is a new encoded picture of old content: give it the acquisition
// time so protocol decoders do not discard it as a duplicate/old PTS. The trace
// still retains the original Present timestamp separately.
func PictureTime(present, acquired time.Time, repeated bool) time.Time {
	if !repeated && !present.IsZero() && !present.After(acquired) {
		return present
	}
	return acquired
}

func (t *Trace) Native(n Native) {
	if t == nil {
		return
	}
	mapTime := func(q int64) time.Time { return QPCTime(q, t.Frequency, t.AnchorQPC, t.Anchor) }
	t.ConversionStart, t.ConversionSubmitted = mapTime(n.ConversionStart), mapTime(n.ConversionEnd)
	t.EncodeSubmit, t.EncodeComplete = mapTime(n.EncodeSubmit), mapTime(n.EncodeComplete)
}

type Delivery struct {
	Trace    *Trace
	Enqueued time.Time
}

func (d Delivery) Enqueue() Delivery {
	if d.Trace != nil {
		d.Enqueued = time.Now()
	}
	return d
}

// Sent records successful writes to the OS/socket, not network delivery or
// client display. Each consumer owns its own enqueue/start timestamps.
func (d Delivery) Sent(protocol string, started time.Time) {
	d.sent(protocol, started, nil)
}

// Waited measures only the selected frame's share of an ACK stall. An old
// capture may have been replaced many times before this frame was enqueued.
func (d Delivery) Waited(started, ended time.Time) time.Duration {
	if d.Enqueued.After(started) {
		started = d.Enqueued
	}
	if ended.Before(started) {
		return 0
	}
	return ended.Sub(started)
}

func (d Delivery) SentRDP(protocol string, started time.Time, processing, sending, ack time.Duration) {
	if d.Trace == nil {
		return
	}
	d.sent(protocol, started, map[string]time.Duration{
		"rdp_prepare_encode": processing,
		"send_call":          sending,
		"ack_wait":           ack,
	})
}

func (d Delivery) sent(protocol string, started time.Time, stages map[string]time.Duration) {
	if d.Trace == nil {
		return
	}
	end := time.Now()
	values := make(map[string]time.Duration)
	for name, value := range stages {
		if value >= 0 {
			values[name] = value
		}
	}
	add(values, "queue_and_prepare", d.Enqueued, started)
	add(values, "ready_to_enqueue", d.Trace.Ready, d.Enqueued)
	add(values, "send_span", started, end)
	add(values, "ready_to_socket", d.Trace.Ready, end)
	if d.Trace.presentationValid() {
		add(values, "present_to_socket", d.Trace.Present, end)
	}
	add(values, "acquire_to_socket", d.Trace.Acquired, end)
	observe("send/"+protocol+"/"+d.Trace.Path, values, d.Trace)
}

func (t *Trace) Capture() {
	if t == nil {
		return
	}
	if t.ID == 0 {
		t.ID = traceSequence.Add(1)
	}
	v := make(map[string]time.Duration)
	if t.presentationValid() {
		add(v, "present_to_acquire", t.Present, t.Acquired)
		add(v, "present_to_ready", t.Present, t.Ready)
	}
	add(v, "acquire_wait", t.AcquireStart, t.Acquired)
	add(v, "copy_submit_cpu", t.Acquired, t.CopySubmitted)
	add(v, "convert_submit_cpu", t.ConversionStart, t.ConversionSubmitted)
	add(v, "copy_to_convert", t.CopySubmitted, t.ConversionStart)
	add(v, "encode_and_wait", t.EncodeSubmit, t.EncodeComplete)
	add(v, "output_to_ready", t.EncodeComplete, t.Ready)
	add(v, "acquire_to_ready", t.Acquired, t.Ready)
	add(v, "capture_to_encode_submit", t.Acquired, t.EncodeSubmit)
	add(v, "capture_encode_queue", t.Acquired, t.TextureReadyStart)
	add(v, "texture_ready_wait", t.TextureReadyStart, t.TextureReadyEnd)
	add(v, "texture_import_cpu", t.TextureReadyEnd, t.TextureImported)
	add(v, "encode_submit_prepare", t.TextureImported, t.EncodeSubmit)
	observe("capture/"+t.Path, v, t)
}

// Validate the source against acquisition once, rather than accepting a future
// source PTS as soon as encoding/sending has taken long enough to catch up.
func (t *Trace) presentationValid() bool {
	return !t.Repeated && !t.Present.IsZero() && !t.Acquired.IsZero() && !t.Present.After(t.Acquired)
}

func add(values map[string]time.Duration, name string, start, end time.Time) {
	if !start.IsZero() && !end.IsZero() && !end.Before(start) {
		values[name] = end.Sub(start)
	}
}

type window struct {
	start  time.Time
	values map[string][]time.Duration
}

var stats = struct {
	sync.Mutex
	windows map[string]*window
}{windows: make(map[string]*window)}

func observe(label string, values map[string]time.Duration, trace *Trace) {
	if len(values) == 0 {
		return
	}
	now := time.Now()
	writeSample(label, values, trace, now)
	stats.Lock()
	w := stats.windows[label]
	if w == nil {
		w = &window{start: now, values: make(map[string][]time.Duration)}
		stats.windows[label] = w
	}
	for name, value := range values {
		// Bound memory even when one label receives many clients' samples.
		if len(w.values[name]) < 1024 {
			w.values[name] = append(w.values[name], value)
		}
	}
	if now.Sub(w.start) < 5*time.Second {
		stats.Unlock()
		return
	}
	delete(stats.windows, label)
	stats.Unlock()
	var names []string
	for name := range w.values {
		names = append(names, name)
	}
	sort.Strings(names)
	var fields []string
	for _, name := range names {
		v := w.values[name]
		sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
		fields = append(fields, fmt.Sprintf("%s_ms=%.3f/%.3f/%.3f(n=%d)", name, ms(quantile(v, 50)), ms(quantile(v, 95)), ms(quantile(v, 99)), len(v)))
	}
	logging.Debugf("video-timing: %s p50/p95/p99 %s (sample=1/%d; host CPU clock)", label, strings.Join(fields, " "), sampleEvery)
}

func quantile(sorted []time.Duration, percent int) time.Duration {
	return sorted[max(0, (len(sorted)*percent+99)/100-1)]
}
func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
