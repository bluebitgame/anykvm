package spice

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"anykvm/internal/capture"
	"anykvm/internal/dmabuf"
	"anykvm/internal/framebuf"
	"anykvm/internal/logging"
)

// A lease includes both FD ownership and upstream draw ownership. Duplicating an
// FD alone cannot prevent the producer from overwriting the displayed pixels.
type frameLease struct {
	frame dmabuf.Frame
	done  func()
}

type nativeSession interface {
	next() (*frameLease, error)
	input(kind, a, b, buttons int) error
	close()
}

type gpuEncoder interface {
	EncodeSource(dmabuf.Frame, bool) ([]byte, error)
	EncodeLast(bool) ([]byte, error)
	Close()
}

type encodeOptions struct {
	major, minor                uint32
	width, height, fps, bitrate int
}

type sessionFactory func(string, string, string) (nativeSession, error)
type encoderFactory func(encodeOptions) (gpuEncoder, error)

type Stats struct {
	GPUFrames, RepeatedFrames uint64
	CPUReadbacks              uint64
	AudioFrames               uint64
}

// Device serializes video encoding while audio uses a separate session lifetime
// lock. Close waits for both GPU completion and active audio reads before freeing
// the session or acknowledging an outstanding scanout.
type Device struct {
	// mu serializes video, input and cursor geometry. Session replacement holds
	// mu followed by sessionMu; audio readers need only sessionMu.
	mu        sync.Mutex
	sessionMu sync.RWMutex

	// Immutable source configuration and backend factories.
	raw         string
	opts        Options
	openSession sessionFactory
	openEncoder encoderFactory

	// Session lifetime and audio enablement are guarded by sessionMu. Video
	// callers may also read session/generation under mu because writers hold both.
	session      nativeSession
	generation   uint64
	audioEnabled bool
	audioFrames  atomic.Uint64

	// Video state, guarded by mu. pending keeps the producer's draw lease alive
	// until the next paced encode; encoder owns the image used for idle keyframes.
	pending                        *frameLease
	encoder                        gpuEncoder
	encoding                       encodeOptions
	stream                         *capture.VideoStreamOptions
	codec                          string
	bitrate, targetW, targetH, fps int
	width, height                  int
	force                          bool
	openedAt, lastFrame            time.Time
	stats                          Stats

	// Input and cursor state, guarded by mu.
	buttons                        int
	keys                           [256]bool
	cursorGeneration, cursorSerial uint64
}

func (d *Device) Open() error { return d.OpenWithFormat(0, 0, 0) }

func (d *Device) OpenWithFormat(w, h, fps int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session != nil {
		return fmt.Errorf("SPICE source is already open")
	}
	if d.codec != "" && d.codec != "h264" {
		return fmt.Errorf("SPICE GPU upstream currently requires H.264 output")
	}
	if d.stream != nil && d.stream.Codec != "" && d.stream.Codec != "h264" {
		return fmt.Errorf("SPICE GPU upstream currently requires H.264 output")
	}
	password := d.opts.Password
	if d.opts.PasswordEnv != "" {
		var ok bool
		password, ok = os.LookupEnv(d.opts.PasswordEnv)
		if !ok {
			return fmt.Errorf("spice.password_env is not set")
		}
	}
	if len(password) > 4096 || strings.ContainsRune(password, 0) {
		return fmt.Errorf("invalid SPICE password")
	}
	path, err := socketPath(d.raw)
	if err != nil {
		return err
	}
	node := d.opts.RenderNode
	if node == "" {
		node = "/dev/dri/renderD128"
	}
	session, err := d.openSession(path, password, node)
	if err != nil {
		return fmt.Errorf("SPICE GPU connect: %w", err)
	}
	d.sessionMu.Lock()
	d.session = session
	d.generation++
	d.audioFrames.Store(0)
	d.cursorSerial = 0
	if s, ok := session.(audioSession); ok {
		s.setAudioEnabled(d.audioEnabled)
	}
	d.sessionMu.Unlock()
	d.targetW, d.targetH = w, h
	d.fps = fps
	if d.fps <= 0 {
		d.fps = 30
	}
	if d.stream != nil && d.stream.FPS > 0 {
		d.fps = d.stream.FPS
	}
	d.force = true
	d.openedAt = time.Now()
	d.lastFrame = time.Time{}
	d.width, d.height, d.buttons = 0, 0, 0
	d.keys = [256]bool{}
	d.stats = Stats{}
	logging.Infof("spice: local GL source; render_node=%s; H.264 DMA-BUF hardware encoding; CPU pixel readback disabled", node)
	return nil
}

func (d *Device) Capture() (*capture.Frame, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session == nil {
		return nil, capture.ErrStreamStopped
	}
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
		if d.encoder != nil && d.force {
			data, err := d.encoder.EncodeLast(true)
			if err != nil {
				return nil, fmt.Errorf("%w: SPICE cached GPU frame: %v", capture.ErrStreamStopped, err)
			}
			d.force = false
			d.stats.RepeatedFrames++
			return d.output(data, time.Now()), nil
		}
		timeout := d.opts.Timeout
		if timeout == 0 {
			timeout = 15 * time.Second
		}
		if d.encoder == nil && time.Since(d.openedAt) > timeout {
			return nil, fmt.Errorf("%w: SPICE did not provide a usable GL scanout within %s; enable local SPICE GL and virtio-gpu scanout (CPU fallback disabled)", capture.ErrStreamStopped, timeout)
		}
		return nil, nil
	}
	f := lease.frame
	if err := f.Validate(); err != nil {
		lease.done()
		return nil, fmt.Errorf("%w: SPICE scanout: %v", capture.ErrStreamStopped, err)
	}
	now := time.Now()
	o := d.options(f)
	if o.width < 2 || o.height < 2 || o.width > 8192 || o.height > 8192 {
		lease.done()
		return nil, fmt.Errorf("%w: invalid SPICE output dimensions", capture.ErrStreamStopped)
	}
	changed := d.encoder == nil || d.encoding != o
	if !changed && !d.force && now.Sub(d.lastFrame) < time.Second/time.Duration(o.fps) {
		// Hold the upstream draw until due. Dropping the last update before an
		// idle desktop would otherwise leave the viewer permanently stale.
		d.pending = lease
		return nil, nil
	}
	defer lease.done() // acknowledge only after GPU completion, including failures
	if changed {
		if d.encoder != nil {
			d.encoder.Close()
			d.encoder = nil
		}
		encoder, err := d.openEncoder(o)
		if err != nil {
			return nil, fmt.Errorf("%w: SPICE DMA-BUF hardware encoder: %v (CPU fallback disabled)", capture.ErrStreamStopped, err)
		}
		d.encoder = encoder
		d.encoding = o
		d.force = true
	}
	data, err := d.encoder.EncodeSource(f, d.force)
	if err != nil {
		return nil, fmt.Errorf("%w: SPICE DMA-BUF encoding: %v (CPU fallback disabled)", capture.ErrStreamStopped, err)
	}
	d.force = false
	d.lastFrame = now
	d.width, d.height = int(f.Width), int(f.Height)
	d.stats.GPUFrames++
	if d.stats.GPUFrames == 1 {
		logging.Infof("spice: first DMA-BUF -> Vulkan Video H.264 frame; source=%dx%d fourcc=%08x modifier=%016x flip_y=%t; cpu_readbacks=0", f.Width, f.Height, f.FourCC, f.Modifier, f.FlipY)
	}
	return d.output(data, now), nil
}

func (d *Device) options(f dmabuf.Frame) encodeOptions {
	w, h := int(f.Width), int(f.Height)
	tw, th := d.targetW, d.targetH
	bitrate := d.bitrate
	if bitrate <= 0 {
		bitrate = 12000
	}
	if d.stream != nil {
		tw, th = d.stream.Width, d.stream.Height
		if d.stream.Bitrate > 0 {
			bitrate = d.stream.Bitrate
		}
	}
	if tw > 0 && th > 0 {
		w, h = tw, th
	}
	return encodeOptions{
		major: f.DeviceMajor, minor: f.DeviceMinor,
		width: w &^ 1, height: h &^ 1,
		fps: d.fps, bitrate: bitrate,
	}
}

func (d *Device) output(data []byte, at time.Time) *capture.Frame {
	buffer := framebuf.New(data, nil)
	return &capture.Frame{
		Format: capture.FormatH264,
		Buffer: buffer, Data: buffer.Bytes(),
		Width: d.encoding.width, Height: d.encoding.height,
		SourceWidth: d.width, SourceHeight: d.height,
		CapturedAt: at,
	}
}

func (d *Device) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sessionMu.Lock()
	if d.session != nil {
		if d.pending != nil {
			d.pending.done()
			d.pending = nil
		}
		_ = d.releaseKeys()
		for button := 1; button <= 3; button++ {
			mask := 1 << uint(button-1)
			if d.buttons&mask != 0 {
				d.buttons &^= mask
				_ = d.session.input(inputButtonRelease, button, 0, d.buttons)
			}
		}
		d.session.close()
		d.cursorSerial = 0
		d.generation++
		d.session = nil
	}
	d.sessionMu.Unlock()
	if d.encoder != nil {
		d.encoder.Close()
		d.encoder = nil
	}
	logging.Infof("spice: closed; gpu_frames=%d repeated_frames=%d audio_frames=%d cpu_readbacks=%d", d.stats.GPUFrames, d.stats.RepeatedFrames, d.audioFrames.Load(), d.stats.CPUReadbacks)
	return nil
}

func (d *Device) Stats() Stats {
	d.mu.Lock()
	defer d.mu.Unlock()
	stats := d.stats
	stats.AudioFrames = d.audioFrames.Load()
	return stats
}
