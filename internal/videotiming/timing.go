// Package videotiming records sampled host-side video latency. CPU submission
// timestamps never imply GPU completion or remote display presentation.
package videotiming

import (
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var Enabled = os.Getenv("ANYKVM_VIDEO_TIMING") == "1"
var sequence atomic.Uint64

// Sample keeps trace allocation and aggregation off most frames. Native QPC
// markers are enabled for the session, but only every 30th picture is reported.
func Sample() bool { return Enabled && (sequence.Add(1)-1)%30 == 0 }

// Native contains QPC ticks, all on the host CPU clock. ConversionEnd is the
// return from GPU work submission, not completion of the conversion on the GPU.
type Native struct {
	ConversionStart, ConversionEnd int64
	EncodeSubmit, EncodeComplete   int64
}

// Trace is populated by capture, then treated as immutable by every consumer.
type Trace struct {
	Present, AcquireStart, Acquired, CopySubmitted time.Time
	ConversionStart, ConversionSubmitted           time.Time
	EncodeSubmit, EncodeComplete, Ready            time.Time
	Anchor                                         time.Time
	AnchorQPC, Frequency                           int64
	Path                                           string
	Repeated                                       bool
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
	if d.Trace == nil {
		return
	}
	end := time.Now()
	values := make(map[string]time.Duration)
	add(values, "queue_and_prepare", d.Enqueued, started)
	add(values, "ready_to_enqueue", d.Trace.Ready, d.Enqueued)
	add(values, "send_span", started, end)
	add(values, "ready_to_socket", d.Trace.Ready, end)
	if !d.Trace.Repeated {
		add(values, "present_to_socket", d.Trace.Present, end)
	}
	observe("send/"+protocol+"/"+d.Trace.Path, values)
}

func (t *Trace) Capture() {
	if t == nil {
		return
	}
	v := make(map[string]time.Duration)
	if !t.Repeated {
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
	observe("capture/"+t.Path, v)
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

func observe(label string, values map[string]time.Duration) {
	if len(values) == 0 {
		return
	}
	now := time.Now()
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
	log.Printf("video-timing: %s p50/p95/p99 %s (sample=1/30; host CPU clock)", label, strings.Join(fields, " "))
}

func quantile(sorted []time.Duration, percent int) time.Duration {
	return sorted[max(0, (len(sorted)*percent+99)/100-1)]
}
func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
