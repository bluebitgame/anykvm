package coredevice

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"anykvm/internal/avmedia"
	"anykvm/internal/capture"
	"anykvm/internal/jpegcodec"
	"anykvm/internal/logging"
	"github.com/google/uuid"
)

// Client is both the capture device and HID sender for one directly connected
// iPhone. A new session is established on each Open, including after Close.
type Client struct {
	udid          string
	options       Options
	inputOnly     bool
	openMu        sync.Mutex
	mu            sync.Mutex
	session       *session
	opening       context.CancelFunc
	width, height atomic.Int64
	orientation   atomic.Int32
	videoCodec    atomic.Int32 // 0: JPEG/BGRA, 1: BGRA, 2: native HEVC.
	decodeImage   atomic.Bool
	jpegQuality   atomic.Int32
	wantAudio     atomic.Bool
}

func NewClient(device string, options Options) (*Client, error) {
	id, err := DeviceUDID(device)
	if err != nil {
		return nil, err
	}
	if err = options.Validate(); err != nil {
		return nil, err
	}
	if options.Timeout == 0 {
		options.Timeout = 60 * time.Second
	}
	c := &Client{udid: id, options: options}
	c.decodeImage.Store(true)
	return c, nil
}

// NewInputClient connects Universal HID independently of DisplayService video.
func NewInputClient(device string, options Options) (*Client, error) {
	c, err := NewClient(device, options)
	if err == nil {
		c.inputOnly = true
	}
	return c, err
}

func (c *Client) SetInputDimensions(width, height int) {
	c.width.Store(int64(width))
	c.height.Store(int64(height))
	if c.inputOnly {
		_ = c.withInput(func(i *inputState) error {
			i.viewWidth, i.viewHeight = width, height
			i.updateDisplayRotation()
			return nil
		})
	}
}
func (c *Client) SetDecodeImage(v bool) { c.decodeImage.Store(v) }
func (c *Client) SetJPEGQuality(v int)  { c.jpegQuality.Store(int32(v)) }
func (c *Client) RequestKeyframe() error {
	c.mu.Lock()
	s := c.session
	c.mu.Unlock()
	if s == nil || s.udp == nil {
		return nil
	}
	peer, err := net.ResolveUDPAddr("udp6", net.JoinHostPort(s.endpoint.address, fmt.Sprint(s.config.sourcePort)))
	if err != nil {
		return err
	}
	return s.sendFeedback(peer, true)
}
func (*Client) SupportsVideoCodec(codec string) bool {
	return codec == "hevc" || codec == "rawvideo" || codec == "mjpeg" || codec == ""
}

// GameStream forwards the phone's native access units. Client resolution,
// bitrate and FPS do not configure the DisplayService encoder.
func (c *Client) SetVideoStreamOptions(options *capture.VideoStreamOptions) {
	if options != nil {
		c.SetVideoCodec(options.Codec)
	}
}

func (*Client) SupportsGameStreamHEVC() bool { return true }

func (c *Client) SetVideoCodec(codec string) {
	switch codec {
	case "hevc":
		c.videoCodec.Store(2)
	case "rawvideo", "uyvy": // The desktop GUI requests UYVY; BGRA is its supported fallback.
		c.videoCodec.Store(1)
	default:
		c.videoCodec.Store(0)
	}
}
func (c *Client) Open() error { return c.OpenWithFormat(0, 0, 0) }
func (c *Client) OpenWithFormat(_, _, _ int) error {
	c.openMu.Lock()
	defer c.openMu.Unlock()
	c.mu.Lock()
	if c.session != nil {
		c.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.options.Timeout)
	c.opening = cancel
	c.mu.Unlock()
	defer func() { cancel(); c.mu.Lock(); c.opening = nil; c.mu.Unlock() }()
	var decoder *avmedia.Session
	var err error
	if !c.inputOnly && c.videoCodec.Load() != 2 {
		// Only consumers requiring pixels/JPEG need FFmpeg; native HEVC does not.
		decoder, err = avmedia.NewDecoder("hevc")
		if err != nil {
			return fmt.Errorf("coredevice: native HEVC decoder: %w", err)
		}
	}
	var s *session
	if c.inputOnly {
		s, err = c.startInput(ctx)
	} else {
		s, err = c.start(ctx, decoder)
	}
	if err != nil {
		if decoder != nil {
			decoder.Close()
		}
		return err
	}
	select {
	case <-s.ready:
	case <-s.done:
		s.close()
		return s.failure()
	case <-ctx.Done():
		s.stop(ctx.Err())
		s.close()
		return fmt.Errorf("coredevice: first video frame: %w", ctx.Err())
	}
	// Do not publish a session cancelled by Close while startup was in flight.
	c.mu.Lock()
	if ctx.Err() != nil {
		c.mu.Unlock()
		s.close()
		return ctx.Err()
	}
	c.session = s
	c.mu.Unlock()
	return nil
}
func (c *Client) Capture() (*capture.Frame, error) {
	if c.inputOnly {
		return nil, fmt.Errorf("%w: HID-only client has no video", capture.ErrStreamStopped)
	}
	c.mu.Lock()
	s := c.session
	c.mu.Unlock()
	if s == nil {
		return nil, capture.ErrStreamStopped
	}
	select {
	case <-s.done:
		return nil, s.failure()
	default:
	}
	select {
	case f := <-s.frames:
		return f, nil
	case <-s.done:
		return nil, s.failure()
	}
}
func (c *Client) Close() error {
	c.mu.Lock()
	s := c.session
	c.session = nil
	cancel := c.opening
	if cancel != nil {
		cancel()
	}
	c.mu.Unlock()
	if s != nil {
		s.close()
	}
	return nil
}

type session struct {
	client                         *Client
	endpoint                       *endpoint
	display                        *xpcChannel
	udp                            *net.UDPConn
	input                          *inputState
	id                             uuid.UUID
	config                         streamConfig
	passthrough, jpegOutput        bool
	frames                         chan *capture.Frame
	ready, done, workerDone        chan struct{}
	readyOnce, stopOnce, closeOnce sync.Once
	errMu                          sync.Mutex
	err                            error
	feedbackMu                     sync.Mutex
	highestSequence                atomic.Uint32
	orientationCancel              context.CancelFunc
	orientationDone                chan struct{}
	audio                          *audioStream
	audioErr                       error
}

func (s *session) sendFeedback(peer *net.UDPAddr, pli bool) error {
	s.feedbackMu.Lock()
	defer s.feedbackMu.Unlock()
	_ = s.udp.SetWriteDeadline(time.Now().Add(time.Second))
	_, err := s.udp.WriteToUDP(s.config.feedback(s.highestSequence.Load(), pli), peer)
	return err
}

func (s *session) failure() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.err != nil {
		return fmt.Errorf("%w: %v", capture.ErrStreamStopped, s.err)
	}
	return capture.ErrStreamStopped
}
func (s *session) stop(err error) {
	s.stopOnce.Do(func() {
		s.errMu.Lock()
		s.err = err
		s.errMu.Unlock()
		close(s.done)
		if s.orientationCancel != nil {
			s.orientationCancel()
		}
		if s.udp != nil {
			s.udp.Close()
		}
		if s.audio != nil {
			s.audio.stop()
		}
	})
}
func (s *session) close() {
	s.closeOnce.Do(func() {
		s.stop(nil)
		if s.audio != nil {
			<-s.audio.done
		}
		if s.orientationDone != nil {
			<-s.orientationDone
		}
		s.input.close()
		if s.display != nil {
			s.endpoint.stopStream(s.display, s.id)
			s.display.Close()
		}
		s.endpoint.Close()
		<-s.workerDone
		for {
			select {
			case f := <-s.frames:
				f.Release()
			default:
				return
			}
		}
	})
}
func (c *Client) start(ctx context.Context, decoder *avmedia.Session) (_ *session, err error) {
	e, err := discover(ctx, c.udid, c.options)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			e.Close()
		}
	}()
	display, err := e.connect(ctx, displayService)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !success {
			display.Close()
		}
	}()
	udp, err := openReceiver(ctx, e.address)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !success {
			udp.Close()
		}
	}()
	id := uuid.New()
	attempted := false
	defer func() {
		if !success && attempted {
			e.stopStream(display, id)
		}
	}()
	// Bind first: the device may stream before the negotiation reply arrives.
	attempted = true
	var audio *audioStream
	var audioErr error
	if c.wantAudio.Load() {
		audio, audioErr = startAudio(ctx, e, id)
		if audioErr != nil {
			logging.Infof("coredevice: audio unavailable: %v", audioErr)
		}
	}
	defer func() {
		if !success && audio != nil {
			audio.stop()
			<-audio.done
		}
	}()
	cfg, err := display.startStream(ctx, e, udp, id)
	if err != nil {
		return nil, err
	}
	input, err := e.openInput(ctx, false)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !success {
			input.close()
		}
	}()
	// DisplayService authenticates touch input asynchronously after negotiation.
	timer := time.NewTimer(300 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	display.live()
	input.channel.live()
	s := &session{client: c, endpoint: e, display: display, udp: udp, id: id, config: cfg,
		frames: make(chan *capture.Frame, 1), ready: make(chan struct{}), done: make(chan struct{}), workerDone: make(chan struct{}),
		input: input,
		audio: audio, audioErr: audioErr,
		passthrough: decoder == nil, jpegOutput: c.videoCodec.Load() == 0 && !c.decodeImage.Load(),
	}
	if s.passthrough {
		logging.Infof("coredevice: native HEVC passthrough requested; no host video decoder or encoder")
	}
	success = true
	s.startOrientationTracking()
	go s.mediaLoop(decoder)
	return s, nil
}

func (c *Client) startInput(ctx context.Context) (*session, error) {
	e, err := discoverServices(ctx, c.udid, c.options, false)
	if err != nil {
		return nil, err
	}
	input, err := e.openInput(ctx, true)
	if err != nil {
		e.Close()
		return nil, err
	}
	input.channel.live()
	s := &session{client: c, endpoint: e, input: input, frames: make(chan *capture.Frame, 1), ready: make(chan struct{}), done: make(chan struct{}), workerDone: make(chan struct{})}
	close(s.ready)
	close(s.workerDone)
	logging.Infof("coredevice: standalone HID ready for %s (feature=%s, mainTouchscreen=257); no DisplayService stream", e.udid, input.channel.hidFeature)
	s.startOrientationTracking()
	return s, nil
}
func (s *session) emit(f *capture.Frame) {
	f.Rotation = displayRotation(s.client.orientation.Load(), f.Width, f.Height)
	if s.passthrough {
		// Compressed deltas depend on previous pictures; preserve order rather
		// than using the newest-picture policy for decoded frames.
		select {
		case s.frames <- f:
			s.readyOnce.Do(func() { close(s.ready) })
		case <-s.done:
			f.Release()
		}
		return
	}
	select {
	case <-s.done:
		f.Release()
		return
	default:
	}
	select {
	case s.frames <- f:
	default:
		select {
		case old := <-s.frames:
			old.Release()
		default:
		}
		select {
		case s.frames <- f:
		default:
			f.Release()
		}
	}
	s.readyOnce.Do(func() { close(s.ready) })
}
func (s *session) mediaLoop(decoder *avmedia.Session) {
	defer close(s.workerDone)
	defer func() {
		if decoder != nil {
			decoder.Close()
		}
	}()
	assembler := hevcAssembler{waiting: true}
	var lastHEVCInfo hevcInfo
	reorder := rtpReorder{}
	buffer := make([]byte, 65535)
	lastPacket := time.Now()
	lastFeedback := time.Time{}
	var highest uint32
	haveSeq := false
	peerIP := net.ParseIP(s.endpoint.address)
	peer := &net.UDPAddr{IP: peerIP, Port: s.config.sourcePort}
	if peerIP == nil {
		resolved, err := net.ResolveUDPAddr("udp6", net.JoinHostPort(s.endpoint.address, fmt.Sprint(s.config.sourcePort)))
		if err != nil {
			s.stop(err)
			return
		}
		peer = resolved
		peerIP = resolved.IP
	}
	failures := 0
	process := func(packets [][]byte, gap bool) error {
		if gap {
			assembler.loss()
		}
		for _, p := range packets {
			au, _, _, err := assembler.feed(p)
			if err != nil {
				assembler.loss()
				continue
			}
			if len(au) == 0 {
				continue
			}
			if s.passthrough {
				info, err := parseSPS(assembler.sets[1])
				if err != nil {
					return fmt.Errorf("HEVC passthrough parameters: %w", err)
				}
				if info != lastHEVCInfo {
					logging.Infof("coredevice: native HEVC parameters %dx%d profile=%d level=%d chroma=%d bit_depth=%d/%d", info.Width, info.Height, info.Profile, info.Level, info.Chroma, info.DepthLuma, info.DepthChroma)
					lastHEVCInfo = info
				}
				s.client.width.Store(int64(info.Width))
				s.client.height.Store(int64(info.Height))
				s.emit(&capture.Frame{Data: au, Format: capture.FormatHEVC, Width: info.Width, Height: info.Height, SourceWidth: info.Width, SourceHeight: info.Height, CapturedAt: time.Now()})
				continue
			}
			frame, err := decoder.Decode(au)
			if err != nil {
				assembler.loss()
				failures++
				if failures >= 3 {
					return fmt.Errorf("HEVC decoding failed: %w", err)
				}
				decoder.Close()
				decoder, err = avmedia.NewDecoder("hevc")
				if err != nil {
					return err
				}
				continue
			}
			if frame == nil {
				continue
			}
			failures = 0
			if frame.Width < 1 || frame.Height < 1 || frame.Width > 8192 || frame.Height > 8192 || len(frame.Data) != frame.Width*frame.Height*4 {
				frame.Release()
				return fmt.Errorf("invalid decoded iPhone frame")
			}
			s.client.width.Store(int64(frame.Width))
			s.client.height.Store(int64(frame.Height))
			if s.jpegOutput {
				quality := int(s.client.jpegQuality.Load())
				if quality <= 0 {
					quality = 80
				}
				data, err := jpegcodec.EncodeBGRA(frame.Data, frame.Width, frame.Height, quality)
				frame.Release()
				if err != nil {
					return fmt.Errorf("JPEG fallback: %w", err)
				}
				s.emit(&capture.Frame{Data: data, Format: capture.FormatJPEG, Width: frame.Width, Height: frame.Height, SourceWidth: frame.Width, SourceHeight: frame.Height, CapturedAt: time.Now()})
				continue
			}
			s.emit(&capture.Frame{Buffer: frame.Buffer, Data: frame.Data, Format: capture.FormatRaw, Width: frame.Width, Height: frame.Height, SourceWidth: frame.Width, SourceHeight: frame.Height, CapturedAt: time.Now()})
		}
		return nil
	}
	for {
		_ = s.udp.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
		n, source, err := s.udp.ReadFromUDP(buffer)
		now := time.Now()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
				s.stop(err)
				return
			}
			if err = process(reorder.flush(now)); err != nil {
				s.stop(err)
				return
			}
		} else {
			accepted := source.IP.Equal(peerIP) && n >= 12 && buffer[0]>>6 == 2 && be32(buffer[8:]) == s.config.remoteSSRC && (buffer[1]&127 == 100 || buffer[1]&127 == 123)
			if accepted {
				p := append([]byte(nil), buffer[:n]...)
				if _, err = rtpHeaderLen(p); err != nil {
					continue
				}
				lastPacket = now
				seq := be16(p[2:])
				if !haveSeq {
					highest = uint32(seq)
					haveSeq = true
				} else {
					diff := int16(seq - uint16(highest))
					if diff > 0 {
						highest += uint32(diff)
					}
				}
				s.highestSequence.Store(highest)
				if err = process(reorder.push(p, now)); err != nil {
					s.stop(err)
					return
				}
			}
		}
		// Continuous RTCP or rejected UDP traffic must not suppress this check.
		if now.Sub(lastPacket) > 10*time.Second {
			s.stop(fmt.Errorf("no accepted video RTP packets for 10s; check the tunnel and DisplayService"))
			return
		}
		if now.Sub(lastFeedback) >= time.Second {
			_ = s.sendFeedback(peer, assembler.waiting)
			lastFeedback = now
		}
	}
}
