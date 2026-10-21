package rdp

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"anykvm/internal/clipboard"
	"anykvm/internal/logging"
	"anykvm/internal/netstats"
)

// ErrHEVCUnavailable prevents accepting an HEVC option without a compatible
// RDP capability negotiation and wire format. Existing HEVC capture/encoder
// support alone is insufficient to send HEVC to an RDP client.
var ErrHEVCUnavailable = errors.New("RDP HEVC output is not implemented; set rdp.codecs.hevc=false")

type Config struct {
	DriveRedirection           DriveRedirectionOptions
	Addr, Username, Password   string
	TLSConfig                  *tls.Config
	Bitrate                    int
	Codec                      string
	AllowedCodecs              *CodecOptions
	EnableHEVC                 bool               // Reserved, default false; true returns ErrHEVCUnavailable.
	DisableClipboard           bool               // default false; disables sharing even with a supplied backend.
	ClipboardOptions           *clipboard.Options // nil defaults to bidirectional sharing for every supported type.
	DisableAutoShortcutMapping bool               // default false; disables conversion for all client platforms.
	ProgressiveQuantization    int                // 6..15, 0 defaults to 6; larger values are coarser.
	ProgressiveRefinement      int                // 0=complete tiles, 1..4 coarse AC FIRST bits then UPGRADE between captures.
	HID                        bridge.HIDSender
	CursorCapture              capture.CursorCapture
	Clipboard                  capture.Clipboard // nil selects the host platform backend.
	// Session startup and display updates run on one worker per client, outside
	// the protocol reader. Disconnect cleanup waits for an in-flight callback.
	OnConnected                func(codec string)
	OnSessionStart             func(codec string, desktop capture.DesktopSize)
	OnDesktopResize            func(desktop capture.DesktopSize) error
	OnDisconnected, OnKeyframe func()
}

// RuntimeConfig contains RDP options that can safely change for newly
// authenticated clients without rebinding the listener.
type RuntimeConfig struct {
	Username, Password      string
	Bitrate                 int
	Codec                   string
	AllowedCodecs           *CodecOptions
	ProgressiveQuantization int
	ProgressiveRefinement   int
}

type Server struct {
	cfg           Config
	runtimeMu     sync.RWMutex
	runtime       RuntimeConfig
	mu            sync.Mutex
	activation    sync.Mutex
	listener      net.Listener
	clients       map[*client]bool
	active        *client
	closed        bool
	wg            sync.WaitGroup
	cursorMu      sync.Mutex
	cursorOpen    bool
	nextSessionID atomic.Uint32
}

type client struct {
	s                                     *Server
	conn                                  net.Conn
	raw                                   net.Conn
	tlsCertificate                        *tls.Certificate
	lifeMu                                sync.Mutex
	sessionStarted                        bool // protected by lifeMu
	data                                  clientData
	writeMu                               sync.Mutex
	done                                  chan struct{}
	closeOnce                             sync.Once
	frames                                chan bridge.Frame
	frameSendMu                           sync.Mutex // serializes admission against shutdown
	framePushMu                           sync.Mutex
	lastSource                            *bridge.Frame // guarded by framePushMu; independent frames only
	outputSize                            atomic.Uint64 // client's requested width/height
	compressedSource                      bool          // guarded by framePushMu; prediction references must be retained
	reset                                 atomic.Bool
	ack                                   chan struct{}
	ackMu                                 sync.Mutex
	sent, acknowledged                    uint32
	suspendAck                            bool
	input                                 inputState
	vcBuf, dvcBuf                         []byte
	vcTotal, dvcTotal                     int
	clipboard                             clipboardState
	drives                                driveState
	audio                                 audioState
	staticWriteMu                         sync.Mutex // keeps virtual-channel message fragments together
	dvcClientVersion                      uint16
	dvcVersion                            uint16
	dvcCompression                        rdp8LiteDecoder
	activated, dvcCaps, gfxCreated, ready bool
	gfxOpen                               bool
	dispCreated, dispOpen                 bool
	dispBuf                               []byte
	dispTotal                             int
	dispCompression                       rdp8LiteDecoder
	pendingDesktop                        *capture.DesktopSize
	desktopMu                             sync.Mutex
	desktopUpdate                         *capture.DesktopSize
	desktopWake                           chan struct{}
	codec                                 displayCodec
	cursorAlpha                           bool
	cursorLimit                           int    // immutable after Confirm Active
	logonUser, logonDomain                string // set only after credentials are validated
	logonSessionID                        uint32
	longCredentials, logonNotified        bool
	// Initialized before videoLoop starts, then owned by that goroutine.
	graphics      graphicsLayout
	scaledMapping bool // negotiated before the frame loop starts
}

func NewServer(cfg Config) (*Server, error) {
	if err := cfg.DriveRedirection.Validate(); err != nil {
		return nil, err
	}
	if cfg.EnableHEVC {
		return nil, ErrHEVCUnavailable
	}
	if cfg.AllowedCodecs != nil {
		options := *cfg.AllowedCodecs
		if err := options.Validate(); err != nil {
			return nil, err
		}
		if options.HEVC {
			return nil, ErrHEVCUnavailable
		}
		cfg.AllowedCodecs = &options
	}
	if cfg.Addr == "" || cfg.Username == "" || cfg.Password == "" {
		return nil, errors.New("RDP requires an address, username, and nonempty password")
	}
	if len(cfg.Username) > 1024 || len(cfg.Password) > 1024 {
		return nil, errors.New("RDP credentials exceed the supported length")
	}
	if cfg.TLSConfig == nil || len(cfg.TLSConfig.Certificates) == 0 {
		return nil, errors.New("RDP requires a TLS certificate")
	}
	cfg.TLSConfig = cfg.TLSConfig.Clone()
	cfg.TLSConfig.MinVersion = tls.VersionTLS12
	if cfg.Bitrate < 0 || cfg.Bitrate > 1000000 {
		return nil, errors.New("invalid RDP bitrate")
	}
	var err error
	cfg.ProgressiveQuantization, err = normalizeProgressiveQuantization(cfg.ProgressiveQuantization)
	if err != nil {
		return nil, err
	}
	if err := validateProgressiveRefinement(cfg.ProgressiveRefinement); err != nil {
		return nil, err
	}
	cfg.Codec = strings.ToLower(strings.TrimSpace(cfg.Codec))
	if cfg.Codec == "" {
		cfg.Codec = "auto"
	}
	switch cfg.Codec {
	case "auto", "h264", "clearcodec", "planar", "progressive":
	default:
		return nil, errors.New("RDP codec must be auto, h264, clearcodec, planar, or progressive")
	}
	options := clipboard.DefaultOptions()
	if cfg.ClipboardOptions != nil {
		options = *cfg.ClipboardOptions
	}
	if err := options.Validate(); err != nil {
		return nil, fmt.Errorf("rdp.clipboard_options.%w", err)
	}
	cfg.ClipboardOptions = &options // Snapshot caller-owned policy before starting any workers.
	if cfg.DisableClipboard {
		cfg.Clipboard = nil
	} else if cfg.Clipboard == nil {
		cfg.Clipboard = capture.NewClipboard()
	}
	if cfg.Clipboard != nil {
		_, rich := cfg.Clipboard.(capture.RichTextClipboard)
		_, images := cfg.Clipboard.(capture.ImageClipboard)
		_, sendFiles := cfg.Clipboard.(capture.FileClipboard)
		_, receiveFiles := cfg.Clipboard.(capture.DeferredFileClipboard)
		enabled := options.Text != clipboard.Disabled || rich && options.RichText != clipboard.Disabled ||
			images && options.Images != clipboard.Disabled ||
			sendFiles && options.Files.Allows(clipboard.ServerToClient) || receiveFiles && options.Files.Allows(clipboard.ClientToServer)
		if !enabled {
			cfg.Clipboard = nil
		}
	}
	return &Server{cfg: cfg, runtime: runtimeConfigFromConfig(cfg), clients: make(map[*client]bool)}, nil
}

func runtimeConfigFromConfig(cfg Config) RuntimeConfig {
	return RuntimeConfig{
		Username: cfg.Username, Password: cfg.Password, Bitrate: cfg.Bitrate, Codec: cfg.Codec, AllowedCodecs: cfg.AllowedCodecs,
		ProgressiveQuantization: cfg.ProgressiveQuantization, ProgressiveRefinement: cfg.ProgressiveRefinement,
	}
}

// UpdateRuntime changes settings used as each new RDP client authenticates and
// negotiates graphics. Existing sessions retain their established codec and
// encoders until reconnect.
func (s *Server) UpdateRuntime(runtime RuntimeConfig) {
	if runtime.AllowedCodecs != nil {
		options := *runtime.AllowedCodecs
		runtime.AllowedCodecs = &options
	}
	s.runtimeMu.Lock()
	s.runtime = runtime
	s.runtimeMu.Unlock()
}

func (s *Server) runtimeConfig() RuntimeConfig {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	// A few protocol unit tests construct a Server directly. Preserve that
	// internal construction pattern while production servers always initialize
	// runtime through NewServer.
	if s.runtime == (RuntimeConfig{}) {
		return runtimeConfigFromConfig(s.cfg)
	}
	return s.runtime
}

func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return net.ErrClosed
	}
	if s.listener != nil {
		return errors.New("RDP server already started")
	}
	l, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return err
	}
	s.listener = l
	if s.cfg.CursorCapture != nil {
		if err := s.cfg.CursorCapture.Open(); err != nil {
			logging.Infof("rdp: cursor capture unavailable: %v", err)
		} else {
			s.cursorOpen = true
		}
	}
	s.wg.Add(1)
	go s.accept(l)
	logging.Basicf("rdp: TLS/RDPGFX server listening on %s", l.Addr())
	return nil
}

func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

func (s *Server) accept(l net.Listener) {
	defer s.wg.Done()
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		c := &client{s: s, raw: conn, done: make(chan struct{}), frames: make(chan bridge.Frame, 1), ack: make(chan struct{}, 1)}
		c.raw = netstats.WrapConn(conn)
		c.conn = tls.Server(c.raw, nlaTLSConfig(s.cfg.TLSConfig, &c.tlsCertificate))
		s.mu.Lock()
		if s.closed || len(s.clients) >= 8 {
			s.mu.Unlock()
			conn.Close()
			continue
		}
		s.clients[c] = true
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			defer c.close()
			if err := c.run(); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				select {
				case <-c.done:
				default:
					logging.Infof("rdp: %s: %v", conn.RemoteAddr(), err)
				}
			}
		}()
	}
}

func (s *Server) CloseClients() {
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	if active != nil {
		active.close()
	}
}

func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	if s.listener != nil {
		s.listener.Close()
	}
	clients := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()
	for _, c := range clients {
		c.close()
	}
	s.wg.Wait()
	s.cursorMu.Lock()
	defer s.cursorMu.Unlock()
	if s.cursorOpen {
		s.cursorOpen = false
		if err := s.cfg.CursorCapture.Close(); err != nil {
			logging.Infof("rdp: cursor capture close: %v", err)
		}
	}
}

// Source dimensions are carried by each access unit; graphics reset is emitted
// atomically with the first independently decodable frame of the new size.
func (*Server) UpdateFramebufferSize(int, int) {}

func (s *Server) PushFrame(f bridge.Frame) {
	s.PushFrameWithStop(f, nil)
}

func (s *Server) PushFrameWithStop(f bridge.Frame, stop <-chan struct{}) {
	f.Timing = f.Timing.Enqueue()
	s.mu.Lock()
	c := s.active
	s.mu.Unlock()
	if c == nil || len(f.Data) > 64<<20 || (f.Surface != nil && (f.Buffer == nil || c.codec != codecProgressive)) {
		return
	}
	select {
	case <-c.done:
		return
	default:
	}
	c.frameSendMu.Lock()
	defer c.frameSendMu.Unlock()
	select {
	case <-c.done:
		return
	default:
	}
	select {
	case <-stop:
		return
	default:
	}
	c.framePushMu.Lock()
	f = f.Retain()
	if c.lastSource != nil {
		c.lastSource.Release()
		c.lastSource = nil
	}
	if !predictionCoded(f.Format) {
		cached := f.Retain()
		c.lastSource = &cached
	}
	if predictionCoded(f.Format) {
		c.compressedSource = true
		c.framePushMu.Unlock()
		select {
		case c.frames <- f:
			return
		case <-c.done:
		case <-stop:
		}
		f.Release()
		return
	}
	defer c.framePushMu.Unlock()
	if !c.compressedSource {
		for {
			select {
			case old := <-c.frames:
				f = mergeProgressiveDamage(old, f)
				old.Release()
			default:
				goto drained
			}
		}
	}
drained:
	select {
	case c.frames <- f:
		return
	default:
	}
	f.Release()
}

func (c *client) close() {
	c.closeOnce.Do(func() {
		// Cancel transport and frame waits immediately, even if a native display
		// switch still holds lifeMu. Cleanup remains ordered after that switch.
		close(c.done)
		c.conn.Close()
		c.closeDrives()
		c.frameSendMu.Lock()
		defer c.frameSendMu.Unlock()
		c.framePushMu.Lock()
		if c.lastSource != nil {
			c.lastSource.Release()
			c.lastSource = nil
		}
		for {
			select {
			case old := <-c.frames:
				old.Release()
			default:
				goto drained
			}
		}
	drained:
		c.framePushMu.Unlock()
		c.lifeMu.Lock()
		defer c.lifeMu.Unlock()
		c.input.release()
		c.s.mu.Lock()
		wasActive := c.s.active == c
		if wasActive {
			c.s.active = nil
		}
		delete(c.s.clients, c)
		c.s.mu.Unlock()
		if wasActive && c.sessionStarted && c.s.cfg.OnDisconnected != nil {
			c.s.cfg.OnDisconnected()
		}
	})
}

func (c *client) write(p []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	for len(p) > 0 {
		n, err := c.conn.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

func (c *client) sendMCS(ch uint16, p []byte) error {
	if len(p) > 32767 {
		return errors.New("MCS packet too large")
	}
	h := []byte{2, 0xf0, 0x80, 0x68, 0, 0, 0, 0, 0x70}
	be.PutUint16(h[4:], serverChannel-1001)
	be.PutUint16(h[6:], ch)
	return c.write(tpkt(join(h, perLength(len(p)), p)))
}

func (c *client) run() (err error) {
	stage := "X.224 negotiation"
	defer func() {
		if err != nil {
			err = fmt.Errorf("%s: %w", stage, err)
		}
	}()
	if err := c.conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		return err
	}
	request, fast, err := readPacket(c.raw)
	if err != nil {
		return err
	}
	if fast || len(request) < 7 || request[1] != 0xe0 || int(request[0])+1 != len(request) {
		return errProtocol
	}
	requested := uint32(0)
	// Negotiation data follows the optional routing token/cookie, and may
	// itself be followed by a 36-byte correlation-information structure.
	n := request[7:]
	if len(n) > 0 && n[0] != 1 {
		end := bytes.Index(n, []byte{'\r', '\n'})
		if end < 0 {
			return errProtocol
		}
		n = n[end+2:]
	}
	if len(n) > 0 {
		if len(n) < 8 || n[0] != 1 || le.Uint16(n[2:]) != 8 {
			return errProtocol
		}
		requested = le.Uint32(n[4:])
		if n[1]&8 != 0 {
			if len(n) != 44 || n[8] != 6 || le.Uint16(n[10:]) != 36 {
				return errProtocol
			}
		} else if len(n) != 8 {
			return errProtocol
		}
	}
	response := []byte{14, 0xd0, 0, 0, 0, 0, 0, 2, 0, 8, 0, 1, 0, 0, 0}
	selected := selectSecurityProtocol(requested)
	if selected == 0 {
		response[7] = 3 // SSL_REQUIRED_BY_SERVER.
		_, _ = c.raw.Write(tpkt(response))
		return errors.New("TLS or NLA RDP security required")
	}
	le.PutUint32(response[11:], selected)
	if _, err := c.raw.Write(tpkt(response)); err != nil {
		return err
	}
	if selected != protocolTLS {
		// Native clients can wait for interactive credentials after negotiation.
		if err := c.conn.SetDeadline(time.Now().Add(2 * time.Minute)); err != nil {
			return err
		}
	}
	stage = "TLS handshake"
	if err := c.conn.(*tls.Conn).Handshake(); err != nil {
		return err
	}
	logging.Infof("rdp: %s: TLS established (requested protocols=%#x, selected=%#x)", c.conn.RemoteAddr(), requested, selected)
	nlaAuthenticated := selected != protocolTLS
	if nlaAuthenticated {
		stage = "NLA/CredSSP authentication"
		if err := c.authenticateNLA(); err != nil {
			return err
		}
		if selected == protocolHybridEx {
			// Early User Authorization Result PDU: AUTHZ_SUCCESS.
			if err := c.write(u32(0)); err != nil {
				return err
			}
		}
		if err := c.conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
			return err
		}
		logging.Infof("rdp: %s: NLA credentials accepted", c.conn.RemoteAddr())
	}
	stage = "MCS connect-initial"
	p, fast, err := readPacket(c.conn)
	if err != nil {
		return err
	}
	if fast {
		return errProtocol
	}
	c.data, err = parseConnect(p)
	if err != nil {
		return err
	}
	stage = "MCS connect-response"
	if err := c.write(tpkt(join([]byte{2, 0xf0, 0x80}, connectResponse(c.data, requested)))); err != nil {
		return err
	}
	attached, authenticated := false, false
	joined := make(map[uint16]bool)
	for {
		switch {
		case c.ready:
			stage = "RDP session"
		case c.activated:
			stage = "RDPGFX negotiation"
		case authenticated:
			stage = "RDP activation"
		default:
			stage = "MCS channel setup"
		}
		p, fast, err = readPacket(c.conn)
		if err != nil {
			return err
		}
		if fast {
			stage = "fast-path input"
			if !c.activated {
				return errors.New("input before session activation")
			}
			if err := c.input.fast(p); err != nil {
				return err
			}
			continue
		}
		if len(p) < 4 || p[0] != 2 || p[1] != 0xf0 || p[2] != 0x80 {
			return errProtocol
		}
		switch p[3] {
		case 4:
			stage = "MCS erect-domain"
			if attached {
				return errProtocol
			}
		case 0x28:
			stage = "MCS attach-user"
			if attached || len(p) != 4 {
				return errProtocol
			}
			attached = true
			if err := c.write(tpkt([]byte{2, 0xf0, 0x80, 0x2e, 0, 0, 1})); err != nil {
				return err
			}
		case 0x38:
			stage = "MCS channel-join"
			if !attached || len(p) != 8 || be.Uint16(p[4:])+1001 != userChannel {
				return errProtocol
			}
			ch := be.Uint16(p[6:])
			if ch != userChannel && ch != ioChannel && (ch < 1004 || int(ch-1004) >= len(c.data.channels)) {
				return errProtocol
			}
			joined[ch] = true
			if err := c.write(tpkt(join([]byte{2, 0xf0, 0x80, 0x3e, 0, 0, 1}, p[6:8], p[6:8]))); err != nil {
				return err
			}
		case 0x64:
			stage = "MCS send-data"
			ch, payload, err := parseMCS(p)
			if err != nil {
				return err
			}
			if !joined[ch] {
				return errors.New("data on an unjoined channel")
			}
			if !authenticated {
				stage = "Client Info"
				if ch != ioChannel {
					return errProtocol
				}
				info, err := parseClientInfo(payload)
				if err != nil {
					return err
				}
				runtime := c.s.runtimeConfig()
				if !nlaAuthenticated && !sameCredentials(info.username, info.password, runtime.Username, runtime.Password) {
					return fmt.Errorf("authentication failed (username=%q, password_present=%t); configure the client's saved account", info.username, info.password != "")
				}
				// NLA already validated delegated credentials. Clients may omit
				// them from Client Info after CredSSP; never authenticate twice.
				if nlaAuthenticated && info.username != "" && !strings.EqualFold(info.username, runtime.Username) {
					return errNLAAuth
				}
				if !nlaAuthenticated {
					c.logonUser, c.logonDomain = info.username, info.domain
				}
				c.logonSessionID = c.s.nextSessionID.Add(1)
				authenticated = true
				logging.Infof("rdp: %s: Client Info accepted (NLA=%t)", c.conn.RemoteAddr(), nlaAuthenticated)
				stage = "licensing completion"
				license := join([]byte{0x80, 0, 0, 0, 0xff, 3, 16, 0}, u32(7), u32(2), u16(4), u16(0))
				if err := c.sendMCS(ioChannel, license); err != nil {
					return err
				}
				stage = "Demand Active"
				if err := c.sendMCS(ioChannel, demandActive(c.data.width, c.data.height)); err != nil {
					return err
				}
			} else if ch == ioChannel {
				stage = "share-control/activation"
				if err := c.handleShare(payload); err != nil {
					return err
				}
			} else if ch == c.data.clipboard && c.s.cfg.Clipboard != nil {
				stage = "CLIPRDR"
				if err := c.handleClipboardVC(payload); err != nil {
					return err
				}
			} else if ch == c.data.drives && c.s.cfg.DriveRedirection.Permissions != 0 {
				stage = "RDPDR"
				if err := c.handleDriveVC(payload); err != nil {
					return err
				}
			} else if ch == c.data.audio {
				stage = "RDPSND"
				if err := c.handleAudioVC(payload); err != nil {
					if !errors.Is(err, errProtocol) {
						return err
					}
					// MS-RDPEA requires malformed audio messages to be ignored.
					c.audio.buf, c.audio.total = nil, 0
				}
			} else if ch == c.data.dvc {
				stage = "DRDYNVC/RDPGFX"
				created, opened := c.gfxCreated, c.gfxOpen
				if err := c.handleVC(payload); err != nil {
					return err
				}
				if !created && c.gfxCreated {
					logging.Infof("rdp: %s: DRDYNVC client version=%d; using version %d", c.conn.RemoteAddr(), c.dvcClientVersion, c.dvcVersion)
				}
				if !opened && c.gfxOpen {
					logging.Infof("rdp: %s: RDPGFX channel opened", c.conn.RemoteAddr())
				}
			}
		case 0x21, 0x20:
			return nil
		default:
			return fmt.Errorf("%w: unsupported MCS PDU %#02x", errProtocol, p[3])
		}
	}
}

func (c *client) handleShare(p []byte) error {
	for len(p) > 0 {
		if len(p) < 6 {
			return fmt.Errorf("%w: truncated share-control header", errProtocol)
		}
		n := int(le.Uint16(p))
		if n < 6 || n > len(p) {
			return fmt.Errorf("%w: share-control length %d, available %d", errProtocol, n, len(p))
		}
		message := p[:n]
		p = p[n:]
		switch le.Uint16(message[2:]) & 15 {
		case 3:
			if c.activated {
				return fmt.Errorf("%w: repeated Confirm Active", errProtocol)
			}
			caps, err := confirmActiveCapabilities(message)
			if err != nil {
				return fmt.Errorf("Confirm Active: %w", err)
			}
			c.cursorAlpha, c.longCredentials = caps.alphaPointer, caps.longCredentials
			macShortcuts, swapLeftModifiers := c.input.configureShortcuts(caps.osMajor, !c.s.cfg.DisableAutoShortcutMapping)
			logging.Infof("rdp: client OS=%s build=%d mac_shortcuts=%t swap_left_win_alt=%t", clientOSName(caps.osMajor, caps.osMinor), c.data.clientBuild, macShortcuts, swapLeftModifiers)
			c.cursorLimit = maxPointerSize
			if caps.largePointer {
				c.cursorLimit = maxLargePointerSize
			}
			c.activated = true
			if err := c.sendMCS(ioChannel, shareData(31, join(u16(1), u16(userChannel)))); err != nil {
				return err
			}
			if err := c.sendMCS(ioChannel, shareData(20, join(u16(4), make([]byte, 6)))); err != nil {
				return err
			}
		case 7:
			if !c.activated || n < 18 || message[15] != 0 {
				return fmt.Errorf("%w: invalid share-data header (activated=%t, length=%d)", errProtocol, c.activated, n)
			}
			body := message[18:]
			switch message[14] {
			case 20:
				if len(body) != 8 {
					return fmt.Errorf("%w: control PDU length %d", errProtocol, len(body))
				}
				if le.Uint16(body) == 1 {
					if err := c.sendMCS(ioChannel, shareData(20, join(u16(2), u16(userChannel), u32(serverChannel)))); err != nil {
						return err
					}
				}
			case 39:
				if len(body) != 8 || c.dvcCaps {
					return fmt.Errorf("%w: font-list length %d, repeated=%t", errProtocol, len(body), c.dvcCaps)
				}
				if err := c.sendMCS(ioChannel, shareData(40, join(u16(0), u16(0), u16(3), u16(4)))); err != nil {
					return err
				}
				if err := c.sendLogonInfo(); err != nil {
					return err
				}
				if err := c.sendMCS(ioChannel, arrowPointer()); err != nil {
					return err
				}
				c.dvcCaps = true
				if err := c.startAudio(); err != nil {
					return err
				}
				// Zero priority charges allow immediate transmission in versions 2 and 3.
				if err := c.sendVC(join([]byte{0x50, 0}, u16(int(dvcMaxVersion)), make([]byte, 8))); err != nil {
					return err
				}
			case 28:
				if c.ready {
					if err := c.input.slow(body); err != nil {
						return err
					}
				}
			case 33, 35:
				c.reset.Store(true)
			case 36:
				return io.EOF
			}
		default:
			return fmt.Errorf("%w: unsupported share-control PDU %#x", errProtocol, le.Uint16(message[2:])&15)
		}
	}
	return nil
}

func (c *client) activate() error {
	c.s.activation.Lock()
	defer c.s.activation.Unlock()
	c.lifeMu.Lock()
	defer c.lifeMu.Unlock()
	c.s.mu.Lock()
	old := c.s.active
	closed := c.s.closed
	c.s.mu.Unlock()
	if closed {
		return net.ErrClosed
	}
	if old != nil {
		old.close()
	}
	select {
	case <-c.done:
		return net.ErrClosed
	default:
	}
	c.s.mu.Lock()
	if c.s.closed {
		c.s.mu.Unlock()
		return net.ErrClosed
	}
	c.s.active = c
	c.s.mu.Unlock()
	// The bridge reserves a consumer before evicting another protocol's client.
	size := capture.DesktopSize{Width: c.data.width, Height: c.data.height, ScalePercent: c.data.scalePercent}
	if c.pendingDesktop != nil {
		size = *c.pendingDesktop
	}
	c.data.width, c.data.height, c.data.scalePercent = size.Width, size.Height, size.ScalePercent
	c.setOutputSize(size.Width, size.Height)
	c.pendingDesktop = nil
	if err := c.conn.SetReadDeadline(time.Time{}); err != nil {
		return err
	}
	// Complete graphics initialization before a potentially slow native mode
	// switch. The first captured frame replaces this surface only if its actual
	// output geometry differs (for example, a fixed video.size).
	if err := c.ensureGraphicsSize(size.Width, size.Height); err != nil {
		return err
	}
	c.desktopWake = make(chan struct{}, 1)
	c.ready = true
	if err := c.startClipboard(); err != nil {
		return err
	}
	if err := c.startDrives(); err != nil {
		return err
	}
	c.startAudioLoop()
	logging.Infof("rdp: %s: authenticated RDPGFX session", c.conn.RemoteAddr())
	c.s.wg.Add(1)
	go func() {
		defer c.s.wg.Done()
		c.desktopLoop(size)
	}()
	c.s.wg.Add(1)
	go func() {
		defer c.s.wg.Done()
		if err := c.videoLoop(); err != nil {
			logging.Infof("rdp: video: %v", err)
			c.close()
		}
	}()
	if c.s.cfg.CursorCapture != nil {
		c.s.wg.Add(1)
		go func() {
			defer c.s.wg.Done()
			c.cursorLoop()
		}()
	}
	return nil
}
