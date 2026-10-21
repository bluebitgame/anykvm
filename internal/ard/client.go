package ard

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"anykvm/internal/avmedia"
	"anykvm/internal/capture"
	"anykvm/internal/logging"
)

const maxPayload = 64 << 20
const maxPixels = 16 << 20
const maxSinglePicturePixels = 8912896

var ErrUnsupported = errors.New("ARD capability unsupported")

type Client struct {
	address string
	opts    Options
	life    sync.Mutex // serializes open, close and input against session replacement
	mu      sync.Mutex
	s       *session
}

type session struct {
	conn                net.Conn
	reader              io.Reader
	standard            bool
	fps                 int
	mu                  sync.Mutex
	w, h                int
	fb                  []byte
	cursor              *capture.CursorState
	frames              chan *capture.Frame
	done, ready         chan struct{}
	stopOnce, readyOnce sync.Once
	err                 error
	lastFrame           time.Time
	mediaWaiting        bool
	workers             sync.WaitGroup
	writeMu             sync.Mutex
	key                 []byte
	accepted            bool
	control             *controlCipher
	// These fields are used only by the control worker.
	configured, displaySelected bool
	reconfigurationRequested    bool
	parsingRFB                  bool
	inputMu                     sync.Mutex
	pressed                     map[uint8]uint32
	x, y                        int
	buttons                     byte
	udpMu                       sync.Mutex
	udp                         map[int]*net.UDPConn
	datagrams                   chan []byte
	mediaMu                     sync.Mutex
	receiver                    *srtpReceiver
	feedback                    *srtcpSender
	localSSRC                   uint32
	generation                  uint64
}

func NewClient(id string, opts Options) (*Client, error) {
	addr, err := ParseAddress(id)
	if err != nil {
		return nil, err
	}
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	return &Client{address: addr, opts: opts.normalized()}, nil
}
func (c *Client) Open() error { return c.OpenWithFormat(0, 0, 0) }
func (c *Client) OpenWithFormat(w, h, fps int) error {
	c.life.Lock()
	defer c.life.Unlock()
	if old := c.current(); old != nil {
		select {
		case <-old.done:
			old.workers.Wait()
		default:
			return nil
		}
		c.mu.Lock()
		c.s = nil
		c.mu.Unlock()
	}
	if c.opts.DisplayMode == "private" {
		return fmt.Errorf("%w: private display mode is not implemented", ErrUnsupported)
	}
	password, err := c.opts.credentials()
	if err != nil {
		return err
	}
	standard := false
	if c.opts.Codec == "h264" {
		if c.opts.Fallback != "vnc" {
			return fmt.Errorf("%w: H.264 negotiation is unverified; choose hevc or permit vnc fallback", ErrUnsupported)
		}
		standard = true
	}
	err = c.openSession(password, standard, w, h, fps)
	if !standard && errors.Is(err, ErrUnsupported) && c.opts.Fallback == "vnc" {
		logging.Infof("ard: High Performance unavailable (%v); using configured VNC fallback", err)
		err = c.openSession(password, true, w, h, fps)
	}
	return err
}
func (c *Client) current() *session { c.mu.Lock(); defer c.mu.Unlock(); return c.s }
func (c *Client) openSession(password string, standard bool, w, h, fps int) error {
	conn, err := net.DialTimeout("tcp", c.address, c.opts.Timeout)
	if err != nil {
		return fmt.Errorf("ard: connect: %w", err)
	}
	s := &session{conn: conn, reader: conn, standard: standard, fps: fps, frames: make(chan *capture.Frame, 2), done: make(chan struct{}), ready: make(chan struct{}), pressed: make(map[uint8]uint32), udp: make(map[int]*net.UDPConn), datagrams: make(chan []byte, 256)}
	conn.SetDeadline(time.Now().Add(c.opts.Timeout))
	if err = s.handshake(c.opts.Username, password); err != nil {
		s.stop(err)
		return err
	}
	if !standard {
		if s.w*s.h > maxSinglePicturePixels {
			s.stop(nil)
			return fmt.Errorf("%w: desktop exceeds the single-picture HEVC limit", ErrUnsupported)
		}
		if err = avmedia.Available(); err != nil {
			s.stop(nil)
			return fmt.Errorf("%w: HEVC decoder unavailable: %v", ErrUnsupported, err)
		}
	}
	if w > 0 || h > 0 {
		logging.Infof("ard: requested %dx%d; mirror source retains remote %dx%d", w, h, s.w, s.h)
	}
	if err = s.initialize(); err != nil {
		s.stop(err)
		return err
	}
	conn.SetDeadline(time.Time{})
	c.mu.Lock()
	c.s = s
	c.mu.Unlock()
	s.workers.Add(1)
	go s.receive()
	if !standard {
		s.workers.Add(1)
		go s.mediaLoop()
	}
	if standard {
		logging.Infof("ard: ordinary RFB fallback active %dx%d", s.w, s.h)
		return nil
	}
	timer := time.NewTimer(c.opts.Timeout)
	defer timer.Stop()
	select {
	case <-s.ready:
		logging.Infof("ard: single-picture HEVC decoded; output=BGRA")
		return nil
	case <-s.done:
		err = s.failure()
	case <-timer.C:
		err = fmt.Errorf("%w: timed out waiting for the first HEVC picture (check server support and symmetric UDP reachability)", ErrUnsupported)
	}
	s.stop(err)
	s.workers.Wait()
	c.mu.Lock()
	if c.s == s {
		c.s = nil
	}
	c.mu.Unlock()
	return err
}

func validSize(w, h int) bool                 { return w > 0 && h > 0 && w <= 65535 && h <= 65535 && w <= maxPixels/h }
func (s *session) read(n int) ([]byte, error) { return readExactly(s.reader, n) }
func (s *session) write(p []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.accepted {
		var err error
		if s.control != nil {
			p, err = s.control.encrypt(p)
		} else {
			p, err = legacyEncrypt(s.key, p)
		}
		if err != nil {
			return err
		}
		if len(p) > 65535 {
			return fmt.Errorf("ard: encrypted message exceeds limit")
		}
		p = append([]byte{byte(len(p) >> 8), byte(len(p))}, p...)
	}
	if err := s.conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	n, err := s.conn.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		s.conn.Close()
	}
	return err
}
func (s *session) stop(err error) {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		close(s.done)
		s.conn.Close()
		s.udpMu.Lock()
		for _, u := range s.udp {
			u.Close()
		}
		s.udpMu.Unlock()
	})
}
func (s *session) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return errors.Join(capture.ErrStreamStopped, s.err)
}
func (s *session) emit(f *capture.Frame) {
	select {
	case <-s.done:
		return
	default:
	}
	s.mu.Lock()
	s.lastFrame = time.Now()
	s.mu.Unlock()
	// Decoded desktops are independent snapshots. Retain the newest two frames.
	select {
	case s.frames <- f:
	default:
		select {
		case <-s.frames:
		default:
		}
		select {
		case s.frames <- f:
		default:
		}
	}
	s.readyOnce.Do(func() { close(s.ready) })
}
func (c *Client) Capture() (*capture.Frame, error) {
	s := c.current()
	if s == nil {
		return nil, capture.ErrStreamStopped
	}
	select {
	case <-s.done:
		return nil, s.failure()
	default:
	}
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-s.done:
		return nil, s.failure()
	case f := <-s.frames:
		select {
		case <-s.done:
			return nil, s.failure()
		default:
			return f, nil
		}
	case <-timer.C:
		return nil, nil
	}
}
func (c *Client) Close() error {
	c.life.Lock()
	defer c.life.Unlock()
	s := c.current()
	if s == nil {
		return nil
	}
	err := s.releaseInput()
	s.stop(nil)
	s.workers.Wait()
	c.mu.Lock()
	if c.s == s {
		c.s = nil
	}
	c.mu.Unlock()
	return err
}
func (*Client) SetDecodeImage(bool)       {}
func (*Client) SetJPEGQuality(int)        {}
func (*Client) PreserveCaptureMode() bool { return true }
func (c *Client) CaptureIdleStatus() (bool, string) {
	s := c.current()
	if s == nil {
		return false, "ard: disconnected"
	}
	select {
	case <-s.done:
		return false, "ard: session terminated"
	default:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastFrame.IsZero() {
		return false, "ard: waiting for first picture"
	}
	mode := "hevc"
	if s.standard {
		mode = "ordinary RFB"
	}
	state := "unchanged desktop"
	if s.mediaWaiting {
		state = "recovering media; waiting for HEVC IRAP"
	}
	return true, fmt.Sprintf("ard: %s %s; last picture %.1fs ago", mode, state, time.Since(s.lastFrame).Seconds())
}
func (c *Client) CaptureCursor() (*capture.CursorState, error) {
	s := c.current()
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cursor == nil {
		return nil, nil
	}
	p := *s.cursor
	p.Image = bytes.Clone(p.Image)
	s.cursor.Changed = false
	return &p, nil
}
