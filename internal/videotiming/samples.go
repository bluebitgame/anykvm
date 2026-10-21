package videotiming

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"anykvm/internal/logging"
)

var sampleFile struct {
	sync.Once
	sync.Mutex
	file *os.File
}

// Raw samples are opt-in diagnostics, separate from normal logs. Each record
// contains timing diagnostics only; no pixels, input events or credentials.
// Signed clock offsets live outside values_ms (which contains durations).
type sourceClockSample struct {
	Source             string   `json:"source,omitempty"`
	Status             string   `json:"status"`
	PTSToAcquireMS     *float64 `json:"pts_to_acquire_ms,omitempty"`
	DisplayToAcquireMS *float64 `json:"display_to_acquire_ms,omitempty"`
}

func (t *Trace) sourceClockSample() sourceClockSample {
	c := sourceClockSample{Source: t.SourceClock, Status: "missing"}
	if t.Repeated {
		c.Status = "repeated"
		return c
	}
	if !t.Acquired.IsZero() {
		if !t.Present.IsZero() {
			delta := ms(t.Acquired.Sub(t.Present))
			c.PTSToAcquireMS = &delta
			c.Status = "valid"
			if delta < 0 {
				c.Status = "future"
			}
		}
		if !t.Display.IsZero() {
			delta := ms(t.Acquired.Sub(t.Display))
			c.DisplayToAcquireMS = &delta
		}
	}
	return c
}

func writeSample(label string, values map[string]time.Duration, trace *Trace, now time.Time) {
	if !Enabled {
		return
	}
	sampleFile.Do(func() {
		if path := os.Getenv("ANYKVM_VIDEO_TIMING_FILE"); path != "" {
			var err error
			sampleFile.file, err = os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				logging.Debugf("video-timing: sample file: %v", err)
			}
		}
	})
	if sampleFile.file == nil {
		return
	}
	metrics := make(map[string]float64, len(values))
	for name, value := range values {
		metrics[name] = ms(value)
	}
	data, err := json.Marshal(struct {
		ID       uint64             `json:"id"`
		Kind     string             `json:"kind"`
		At       int64              `json:"at_unix_ms"`
		Repeated bool               `json:"repeated"`
		Clock    sourceClockSample  `json:"source_clock"`
		Values   map[string]float64 `json:"values_ms"`
	}{trace.ID, label, now.UnixMilli(), trace.Repeated, trace.sourceClockSample(), metrics})
	if err != nil {
		return
	}
	sampleFile.Lock()
	_, _ = sampleFile.file.Write(append(data, '\n'))
	sampleFile.Unlock()
}
