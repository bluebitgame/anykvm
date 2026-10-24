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
	"anykvm/internal/logging"
)

// A lease carries a local GPU draw or an owned remote bitmap snapshot. Local
// leases include upstream draw ownership: duplicating the FD alone cannot
// prevent the producer from overwriting pixels before encoding completes.
type frameLease struct {
	frame  dmabuf.Frame
	done   func()
	bitmap *bitmapFrame
	reset  bool
}

type nativeSession interface {
	next() (*frameLease, error)
	input(kind, a, b, buttons int) error
	close()
}

// Session factories receive an address, password and render node. An empty
// render node selects TCP; a non-empty node selects the local GL socket path.
type sessionFactory func(string, string, string) (nativeSession, error)

type Stats struct {
	GPUFrames, RepeatedFrames uint64
	BitmapFrames              uint64
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
	newPipeline pipelineFactory

	// Session lifetime and audio enablement are guarded by sessionMu. Video
	// callers may also read session/generation under mu because writers hold both.
	session      nativeSession
	generation   uint64
	audioEnabled bool
	audioFrames  atomic.Uint64

	// Remote bitmap state, guarded by mu. Keep an owned idle snapshot and the
	// start of the current wait for a primary surface (initial open or reset).
	remote       bool
	lastBitmap   *bitmapFrame
	bitmapWaitAt time.Time

	// Local video state, guarded by mu. pipeline keeps viewer settings across
	// reopen; activePipeline is selected on the first draw and closed per session.
	// pending retains the producer's draw while pacing or GPU admission defers it.
	pending        *frameLease
	activePipeline localPipeline
	pipeline       *capture.DMABUFEncoder

	// Viewer settings and current frame metadata, guarded by mu.
	stream              *capture.VideoStreamOptions
	codec               string
	fps                 int
	width, height       int
	force               bool
	openedAt, lastFrame time.Time
	stats               Stats

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
	e, err := parseEndpoint(d.raw)
	if err != nil {
		return err
	}
	d.remote = e.remote()
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
	if d.remote && d.codec != "" && d.codec != "rawvideo" {
		return fmt.Errorf("SPICE remote capture produces raw video")
	}
	node := d.opts.RenderNode
	if node == "" {
		node = "/dev/dri/renderD128"
	}
	if d.remote {
		node = ""
	}
	session, err := d.openSession(e.address(), password, node)
	if err != nil {
		return fmt.Errorf("SPICE connect: %w", err)
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
	d.lastBitmap = nil
	d.bitmapWaitAt = d.openedAt
	if d.remote {
		logging.Infof("spice: remote display source; decoded BGRA frames")
	} else {
		d.sharedPipeline().Configure(w, h, d.fps)
		logging.Infof("spice: local GL source; render_node=%s; shared DMA-BUF desktop encoding", node)
	}
	return nil
}

func (d *Device) Capture() (*capture.Frame, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session == nil {
		return nil, capture.ErrStreamStopped
	}
	if d.remote {
		return d.captureBitmap()
	}
	return d.captureGL()
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
	d.lastBitmap = nil
	if d.activePipeline != nil {
		native := d.activePipeline.Stats()
		d.stats.GPUFrames, d.stats.CPUReadbacks = native.GPUFrames, native.CPUReadbacks
		_ = d.activePipeline.Close()
		d.activePipeline = nil
	}
	logging.Infof("spice: closed; gpu_frames=%d bitmap_frames=%d repeated_frames=%d audio_frames=%d cpu_readbacks=%d", d.stats.GPUFrames, d.stats.BitmapFrames, d.stats.RepeatedFrames, d.audioFrames.Load(), d.stats.CPUReadbacks)
	return nil
}

func (d *Device) Stats() Stats {
	d.mu.Lock()
	defer d.mu.Unlock()
	stats := d.stats
	if d.activePipeline != nil {
		native := d.activePipeline.Stats()
		stats.GPUFrames, stats.CPUReadbacks = native.GPUFrames, native.CPUReadbacks
	}
	stats.AudioFrames = d.audioFrames.Load()
	return stats
}
