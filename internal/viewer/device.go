package viewer

import (
	"sync"
	"sync/atomic"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
)

// Device keeps only the latest frame so the GUI cannot delay capture.
type Device struct {
	mu        sync.Mutex
	frame     *capture.Frame
	ready     chan struct{}
	done      chan struct{}
	closed    bool
	takenOver atomic.Bool
	Start     func()
	Audio     func([]byte)
}

func NewDevice() *Device      { return &Device{ready: make(chan struct{}, 1), done: make(chan struct{})} }
func (d *Device) Open() error { return d.OpenWithFormat(0, 0, 0) }
func (d *Device) OpenWithFormat(int, int, int) error {
	if d.Start != nil {
		d.Start()
	}
	return nil
}
func (d *Device) SetDecodeImage(bool)            {}
func (d *Device) SetJPEGQuality(int)             {}
func (d *Device) UpdateFramebufferSize(int, int) {}
func (d *Device) CloseClients()                  { d.takenOver.Store(true); d.Close() }
func (d *Device) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.closed {
		d.closed = true
		close(d.done)
	}
	if d.frame != nil {
		d.frame.Release()
		d.frame = nil
	}
	return nil
}
func (d *Device) PushFrame(f bridge.Frame) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	owned := f.Retain()
	frame := &capture.Frame{
		Buffer: owned.Buffer, Data: owned.Data, Format: owned.Format,
		PixFmt: owned.PixFmt, Width: owned.Width, Height: owned.Height,
		Rotation: owned.Rotation, SourceWidth: owned.SourceWidth, SourceHeight: owned.SourceHeight,
		ContentRect: owned.ContentRect, CapturedAt: owned.CapturedAt, PresentationPTS: owned.PresentationPTS,
	}
	if d.frame != nil {
		d.frame.Release()
	}
	d.frame = frame
	select {
	case d.ready <- struct{}{}:
	default:
	}
}
func (d *Device) Capture() (*capture.Frame, error) {
	select {
	case <-d.done:
		return nil, capture.ErrStreamStopped
	case <-d.ready:
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, capture.ErrStreamStopped
	}
	frame := d.frame
	d.frame = nil
	return frame, nil
}
func (d *Device) PushAudio(f capture.AudioFrame) {
	if d.Audio != nil && len(f.PCM) > 0 {
		d.Audio(f.PCM)
	}
}
func (d *Device) Done() <-chan struct{} { return d.done }

func (d *Device) TakenOver() bool { return d.takenOver.Load() }
