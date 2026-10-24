package spice

import (
	"fmt"
	"time"

	"anykvm/internal/capture"
	"anykvm/internal/dmabuf"
)

// localPipeline uses the same format routing and GPU implementation as KMS.
// The interface lets offline tests exercise draw ownership without a GPU.
type localPipeline interface {
	EncodeSource(source dmabuf.Frame, width, height int, at time.Time, pts int64) (*capture.Frame, error)
	Repeat() (*capture.Frame, error)
	Stats() capture.DMABUFStats
	Close() error
}

type pipelineFactory func(*capture.DMABUFEncoder) (localPipeline, error)

// captureGL runs under Device.mu. A deferred draw stays leased; a completed or
// failed draw is acknowledged only after the synchronous GPU call returns.
func (d *Device) captureGL() (*capture.Frame, error) {
	lease := d.pending
	d.pending = nil
	var err error
	if lease == nil {
		lease, err = d.session.next()
	}
	if err != nil {
		return nil, fmt.Errorf("%w: SPICE: %v", capture.ErrStreamStopped, err)
	}
	if lease == nil {
		return d.refreshGL()
	}
	f := lease.frame
	if err := f.Validate(); err != nil {
		lease.done()
		return nil, fmt.Errorf("%w: SPICE scanout: %v", capture.ErrStreamStopped, err)
	}
	now := time.Now()
	changed := int(f.Width) != d.width || int(f.Height) != d.height
	if !changed && !d.force && now.Sub(d.lastFrame) < time.Second/time.Duration(d.fps) {
		// Keep the final draw before an idle desktop until it is due.
		d.pending = lease
		return nil, nil
	}
	// ACK completed/failed work. A deferred draw remains owned by pending and
	// will be acknowledged by a later Capture or Close.
	defer func() {
		if d.pending != lease {
			lease.done()
		}
	}()
	if d.activePipeline == nil {
		if d.newPipeline != nil {
			d.activePipeline, err = d.newPipeline(d.sharedPipeline())
		} else {
			d.activePipeline = d.sharedPipeline()
		}
		if err != nil {
			return nil, fmt.Errorf("%w: SPICE DMA-BUF pipeline: %v", capture.ErrStreamStopped, err)
		}
	}
	if changed || d.force {
		_ = d.sharedPipeline().RequestKeyframe()
	}
	out, err := d.activePipeline.EncodeSource(f, int(f.Width), int(f.Height), now, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: SPICE DMA-BUF encoding: %v", capture.ErrStreamStopped, err)
	}
	if out == nil {
		// Admission/pacing can defer an input. Retain ownership until accepted;
		// never ACK the last update and leave the viewer permanently stale.
		d.pending = lease
		return nil, nil
	}
	d.force = false
	d.lastFrame = now
	d.width, d.height = int(f.Width), int(f.Height)
	return out, nil
}

// refreshGL serves an explicit refresh from owned storage, or waits for the
// first scanout. It never borrows a draw that was already acknowledged.
func (d *Device) refreshGL() (*capture.Frame, error) {
	if d.activePipeline != nil && d.force {
		frame, err := d.activePipeline.Repeat()
		if err != nil {
			return nil, fmt.Errorf("%w: SPICE cached GPU frame: %v", capture.ErrStreamStopped, err)
		}
		d.force = false
		if frame != nil {
			d.stats.RepeatedFrames++
		}
		return frame, nil
	}
	timeout := d.opts.Timeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	if d.width == 0 && time.Since(d.openedAt) > timeout {
		return nil, fmt.Errorf("%w: SPICE did not provide a usable GL scanout within %s; enable local SPICE GL and virtio-gpu scanout", capture.ErrStreamStopped, timeout)
	}
	return nil, nil
}
