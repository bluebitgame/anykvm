package vnc

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"sync"

	"anykvm/internal/capture"
	"anykvm/internal/framebuf"
	"anykvm/internal/videotiming"
)

// Config configures the VNC server.
type Config struct {
	Codecs               *CodecOptions // nil enables all optional encodings
	Addr                 string
	Width                int
	Height               int
	FPS                  int // capture frame rate, for H.264 encoder; 0=default 30
	Name                 string
	Password             string // empty means no authentication
	TLSCertFile          string // TLS cert (PEM); enables VeNCrypt (X.509) encryption
	TLSKeyFile           string // TLS key (PEM)
	VNCTLS               bool   // explicitly enable VeNCrypt (requires TLSCertFile/TLSKeyFile)
	PixelTol             int    // pixel tolerance: |dB|+|dG|+|dR| <= this value = unchanged; 0=exact match
	JPEGQuality          int    // lock JPEG quality (1-100); 0=use client-negotiated value
	ForceContinuous      bool   // force server to proactively push frames, ignoring client requests
	CursorCapture        capture.CursorCapture
	OnClientConnecting   func()     // called before adding a new client (for cross-protocol arbitration)
	OnClientConnected    func()     // called when the first client connects
	OnClientDisconnected func()     // called when the last client disconnects
	OnPixelNeedChanged   func(bool) // called when active client's pixel requirement changes
	OnVideoH264Changed   func(bool) // called when active client's H.264 preference changes (drives capture codec switch)
	MouseMode            string     // "auto" | "absolute" | "relative"
	HID                  HIDSender
}

// HIDSender is the interface for sending keyboard/mouse events to a HID device.
type HIDSender interface {
	KeyDown(keycode uint8) error
	KeyUp() error
	KeyRelease(keycode uint8) error
	MouseMove(dx, dy int) error
	MouseMoveRel(dx, dy int) error
	MouseButton(button int, down bool) error
	MouseWheel(delta int) error
}

// Frame is one video frame: a single format tag + shared byte buffer (single-client model)
type Frame struct {
	Timing videotiming.Delivery
	Buffer *framebuf.Buffer
	Format capture.VideoFormat
	Data   []byte
	Width  int
	Height int
}

// Server is the VNC server.
type Server struct {
	config    Config
	tlsConfig *tls.Config // when non-nil, VeNCrypt (X.509) is offered to clients
	initErr   error
	ln        net.Listener
	mu        sync.Mutex
	cur       *client // current single client (global single-client model)
}

// NewServer creates a VNC server instance.
func NewServer(cfg Config) *Server {
	if cfg.Codecs != nil {
		policy := *cfg.Codecs
		cfg.Codecs = &policy
	}
	if cfg.Addr == "" {
		cfg.Addr = ":5900"
	}
	if cfg.Name == "" {
		cfg.Name = "AnyKVM"
	}
	s := &Server{
		config: cfg,
	}
	if cfg.VNCTLS && (cfg.TLSCertFile == "" || cfg.TLSKeyFile == "") {
		s.initErr = fmt.Errorf("vnc: TLS certificate and key are required when TLS is enabled")
	} else if cfg.VNCTLS {
		cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			s.initErr = fmt.Errorf("vnc: TLS certificate: %w", err)
		} else {
			s.tlsConfig = &tls.Config{
				Certificates: []tls.Certificate{cert},
				MinVersion:   tls.VersionTLS12,
			}
		}
	}
	return s
}

// Start begins the VNC server.
func (s *Server) Start() error {
	if s.initErr != nil {
		return s.initErr
	}
	var err error
	s.ln, err = net.Listen("tcp", s.config.Addr)
	if err != nil {
		return fmt.Errorf("vnc: listen error: %w", err)
	}
	if s.config.CursorCapture != nil {
		if err := s.config.CursorCapture.Open(); err != nil {
			log.Printf("vnc: cursor capture unavailable: %v", err)
		}
	}
	go s.acceptLoop()
	return nil
}

// PushFrame delivers one frame to the current client.
func (s *Server) PushFrame(frame Frame) {
	s.PushFrameWithStop(frame, nil)
}

// PushFrameWithStop cancels backpressure when a capture session ends.
func (s *Server) PushFrameWithStop(frame Frame, stop <-chan struct{}) {
	fd := &frameData{
		timing: frame.Timing.Enqueue(),
		buffer: frame.Buffer,
		format: frame.Format,
		data:   frame.Data,
		width:  uint16(frame.Width),
		height: uint16(frame.Height),
	}

	s.mu.Lock()
	c := s.cur
	s.mu.Unlock()

	if c != nil {
		c.offerFrameWithStop(fd, stop)
	}
}

// PushAudio sends PCM audio data to the current client if it has enabled
// QEMU Audio. Implements bridge.AudioSink.
func (s *Server) PushAudio(a capture.AudioFrame) {
	s.mu.Lock()
	c := s.cur
	s.mu.Unlock()

	if c == nil {
		return
	}
	c.pushQEMUAudio(a)
}

// Close shuts down the VNC server.
func (s *Server) Close() error {
	s.mu.Lock()
	c := s.cur
	s.mu.Unlock()
	if c != nil {
		c.close()
	}

	if s.ln != nil {
		return s.ln.Close()
	}
	return nil
}

// CloseClients disconnects the current client (for VNC/RDP cross-protocol kick).
func (s *Server) CloseClients() {
	s.mu.Lock()
	c := s.cur
	s.mu.Unlock()
	if c != nil {
		c.close()
	}
}

func (s *Server) addClient(c *client) {
	// Notify the bridge so it can kick the other protocol's clients.
	if s.config.OnClientConnecting != nil {
		s.config.OnClientConnecting()
	}

	s.mu.Lock()
	old := s.cur
	s.cur = c
	s.mu.Unlock()

	// Single-client model: disconnect the previous client.
	if old != nil && old != c {
		old.close()
	}

	// old == nil means no previous client, so this is the first connection.
	// OnClientConnected (which starts capture) is deferred until the client has
	// advertised its encodings (see client.markReady), so capture opens directly
	// in the negotiated codec (e.g. H.264) instead of MJPEG first then switching.
	c.firstConsumer = old == nil
	log.Printf("vnc: client added (%s)", c.conn.RemoteAddr())
}

// clientReady fires OnClientConnected (which starts capture) once the given
// client has negotiated its encodings. Only the first consumer triggers it, to
// stay balanced with OnClientDisconnected.
func (s *Server) clientReady(c *client) {
	if !c.firstConsumer {
		return
	}
	if s.config.OnClientConnected != nil {
		s.config.OnClientConnected()
	}
}

func (s *Server) removeClient(c *client) {
	s.mu.Lock()
	isCurrent := s.cur == c
	if isCurrent {
		s.cur = nil
	}
	s.mu.Unlock()

	// Only fire disconnect when the current client leaves; a kicked stale client
	// must not trigger it.
	if isCurrent && s.config.OnClientDisconnected != nil {
		s.config.OnClientDisconnected()
	}
}

// UpdateFramebufferSize records the new framebuffer dimensions used for
// ServerInit of future clients. Existing clients are notified of the size
// change in-band, ordered before the next frame's pixel data (see
// client.sendFrame), which avoids racing writes on the connection.
func (s *Server) UpdateFramebufferSize(width, height int) {
	s.mu.Lock()
	s.config.Width = width
	s.config.Height = height
	s.mu.Unlock()

	log.Printf("vnc: framebuffer size set to %dx%d", width, height)
}

// SetFPS records the effective capture frame rate used to configure the H.264
// encoder (input framerate and GOP length). It is set by the bridge once the
// capture device's rate is known, allowing fps=0 (device default) to resolve to
// the real rate instead of a hard-coded fallback.
func (s *Server) SetFPS(fps int) {
	s.mu.Lock()
	s.config.FPS = fps
	s.mu.Unlock()
}

// UpdateEncoding changes the per-client bitmap encoding policy. It is used for
// clients accepted after the update; established VNC sessions retain their
// negotiated settings until they reconnect.
func (s *Server) UpdateEncoding(pixelTol, jpegQuality int, forceContinuous bool) {
	s.mu.Lock()
	s.config.PixelTol = pixelTol
	s.config.JPEGQuality = jpegQuality
	s.config.ForceContinuous = forceContinuous
	s.mu.Unlock()
}

// UpdateCodecs applies to new connections; existing clients retain their policy.
func (s *Server) UpdateCodecs(policy CodecOptions) {
	s.mu.Lock()
	s.config.Codecs = &policy
	s.mu.Unlock()
}

// fps returns the effective capture frame rate under lock.
func (s *Server) fps() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config.FPS
}

// ClientCount returns the number of connected clients (0 or 1).
func (s *Server) ClientCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil {
		return 1
	}
	return 0
}

// notifyPixelNeed reports the current client's pixel requirement upward.
func (s *Server) notifyPixelNeed(need bool) {
	if s.config.OnPixelNeedChanged != nil {
		s.config.OnPixelNeedChanged(need)
	}
}

// notifyVideoH264 reports whether the current client wants H.264 video upward,
// so the capture layer can switch its output codec.
func (s *Server) notifyVideoH264(want bool) {
	if s.config.OnVideoH264Changed != nil {
		s.config.OnVideoH264Changed(want)
	}
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		log.Printf("vnc: client connected from %s", conn.RemoteAddr())
		s.mu.Lock()
		pixelTol, jpegLock, forceContinuous, cursorCap := s.config.PixelTol, s.config.JPEGQuality, s.config.ForceContinuous, s.config.CursorCapture
		codecs := s.config.Codecs
		s.mu.Unlock()

		c := &client{
			codecs:          codecs,
			conn:            conn,
			server:          s,
			wake:            make(chan struct{}, 1),
			done:            make(chan struct{}),
			pixelFmt:        defaultPixelFormat(),
			pixelTol:        pixelTol,
			jpegLock:        jpegLock,
			forceContinuous: forceContinuous,
			mouseSkip:       true,
			cursorCap:       cursorCap,
		}
		c.h264QCond = sync.NewCond(&c.stateMu)
		go c.run()
	}
}
