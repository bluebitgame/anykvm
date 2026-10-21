package wsmedia

import (
	"crypto/subtle"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"anykvm/internal/av1"
	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"anykvm/internal/framebuf"
	"anykvm/internal/logging"
	"anykvm/internal/netstats"

	"github.com/gorilla/websocket"
)

// Config configures the WebSocket media server.
type Config struct {
	HTTPVideoBufferFrames  int
	HTTPSVideoBufferFrames int
	HTTPAddr               string // HTTP listen address, e.g. ":8080" (empty=disable HTTP)
	HTTPSAddr              string // HTTPS listen address, e.g. ":8443" (empty=disable HTTPS)
	Name                   string
	User                   string // optional HTTP Basic username for web control; empty accepts any username for compatibility
	Password               string // optional shared password, authenticated with HTTP Basic
	AdminPassword          string // protects AdminHandler with HTTP Basic user "admin"
	AdminHandler           http.Handler
	Targets                []Target
	// TargetCover renders the shared GameStream cover for a target platform.
	TargetCover      func(platform string) []byte
	SettingsEnabled  bool
	OnTargetSelected func(index int) error // called only after a browser opens a target WebSocket
	// TLSCertFile/TLSKeyFile point to a PEM cert+key; when HTTPSAddr is set but
	// these are empty, an in-memory self-signed certificate is generated.
	TLSCertFile string
	TLSKeyFile  string
	// OnClientConnecting fires before a new client is added, allowing the
	// bridge to enforce single-client arbitration (kicks existing VNC/RDP).
	OnClientConnecting func()
	// OnClientConnected/Disconnected let the bridge track consumers so capture
	// starts/stops. They fire on first-connect / last-disconnect only.
	OnClientConnected    func()
	OnClientDisconnected func()
	// HID receives keyboard/mouse events from the browser (may be nil).
	HID HIDSender
	// MouseMode reports the active target mode at connection setup.
	MouseMode func() string
	// OnVideoH264Changed reports whether the active browser can decode H.264,
	// so the capture layer can switch its output codec.
	OnVideoH264Changed func(bool)
	// OnVideoHEVCChanged reports whether the active browser can decode HEVC,
	// so the capture layer can switch its output codec.
	OnVideoHEVCChanged       func(bool)
	DisableH264              bool // H.264 remains enabled by default for existing embedders
	DisableHEVC              bool // HEVC remains enabled by default for existing embedders
	EnableAV1                bool // explicit opt-in; browser support alone must not enable AV1
	OnVideoAV1Changed        func(bool)
	OnVideoCodecsChanged     func(h264, hevc, av1 bool)
	OnVideoKeyframeRequested func()
	BrowserMedia             capture.BrowserMediaSource
}

// Target is a selectable browser-control destination. Platform selects the
// shared GameStream cover artwork and is never exposed to the browser.
type Target struct {
	Name, Platform string
}

// HIDSender is the interface for injecting keyboard/mouse events. It matches the
// VNC/RDP HID interface so the same device satisfies all of them.
type HIDSender interface {
	KeyDown(keycode uint8) error
	KeyUp() error
	KeyRelease(keycode uint8) error
	MouseMove(x, y int) error
	MouseMoveRel(dx, dy int) error
	MouseButton(button int, down bool) error
	MouseWheel(delta int) error
}

var vncBtnToHID = [3]int{1, 3, 2}

type metadata struct {
	MouseMode         string `json:"mouseMode,omitempty"`         // connection setting; later metadata may omit it
	VideoBufferFrames int    `json:"videoBufferFrames,omitempty"` // sent at connection setup; later metadata preserves it
	BufferH264        bool   `json:"bufferH264"`                  // opt-in source PTS pacing for native AirPlay
	Width             int    `json:"w"`
	Height            int    `json:"h"`
	Rotation          int    `json:"rotation"` // clockwise; w/h still describe the encoded buffer
	SampleRate        int    `json:"sampleRate"`
	Channels          int    `json:"channels"`
	AudioAvailable    bool   `json:"audioAvailable"`
	Name              string `json:"name"`
}

// Server streams JPEG/H.264/HEVC/AV1 video and PCM or negotiated AAC audio over WebSocket.
// Participates in single-client arbitration (ClaimWS) like VNC/RDP.
type Server struct {
	cfg      Config
	upgrader websocket.Upgrader
	httpSrv  *http.Server
	httpsSrv *http.Server

	accessMu                          sync.RWMutex
	viewerUser                        string
	viewerPass                        string
	adminPassword                     string
	httpVideoBufferFrames             int
	httpsVideoBufferFrames            int
	targetsMu                         sync.RWMutex
	targets                           []Target
	codecsMu                          sync.RWMutex
	enableH264, enableHEVC, enableAV1 bool

	mu            sync.Mutex
	clients       map[*wsClient]struct{}
	meta          metadata
	activeClients int
	lifecycleMu   sync.Mutex // serialize capture start/stop callbacks.
}

func NewServer(cfg Config) *Server {
	if cfg.Name == "" {
		cfg.Name = "AnyKVM"
	}
	return &Server{
		cfg:                    cfg,
		viewerUser:             cfg.User,
		viewerPass:             cfg.Password,
		adminPassword:          cfg.AdminPassword,
		httpVideoBufferFrames:  cfg.HTTPVideoBufferFrames,
		httpsVideoBufferFrames: cfg.HTTPSVideoBufferFrames,
		targets:                append([]Target(nil), cfg.Targets...),
		enableHEVC:             !cfg.DisableHEVC,
		enableH264:             !cfg.DisableH264,
		enableAV1:              cfg.EnableAV1,
		clients:                make(map[*wsClient]struct{}),
		meta: metadata{
			Name:       cfg.Name,
			SampleRate: 48000,
			Channels:   2,
		},
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 4 * 1024,
		},
	}
}

// UpdateAccess changes Web and settings authentication without restarting the
// listeners. Existing WebSocket connections remain connected; future HTTP and
// WebSocket requests use the replacement credentials.
func (s *Server) UpdateAccess(user, password, adminPassword string) {
	s.accessMu.Lock()
	s.viewerUser, s.viewerPass, s.adminPassword = user, password, adminPassword
	s.accessMu.Unlock()
}

// UpdateVideoBufferFrames changes the display queue advertised to future Web
// clients. Connected browsers retain their existing queue until reconnect.
func (s *Server) UpdateVideoBufferFrames(frames int) {
	s.mu.Lock()
	s.httpVideoBufferFrames, s.httpsVideoBufferFrames = frames, frames
	s.mu.Unlock()
}

// UpdateVideoCodecs changes optional codec policy for future Web clients.
// Existing connections retain the codec selected at their initial handshake.
func (s *Server) UpdateVideoCodecs(h264, hevc, av1 bool) {
	s.codecsMu.Lock()
	s.enableH264 = h264
	s.enableHEVC, s.enableAV1 = hevc, av1
	s.codecsMu.Unlock()
}

// UpdateTargets replaces the Web landing-page targets. Connected viewers are
// left to the bridge, which disconnects them before rebuilding target devices.
func (s *Server) UpdateTargets(targets []Target) {
	s.targetsMu.Lock()
	s.targets = append(s.targets[:0], targets...)
	s.targetsMu.Unlock()
}

func (s *Server) webTargets() []Target {
	s.targetsMu.RLock()
	defer s.targetsMu.RUnlock()
	return append([]Target(nil), s.targets...)
}

func (s *Server) access() (user, password, adminPassword string) {
	s.accessMu.RLock()
	defer s.accessMu.RUnlock()
	return s.viewerUser, s.viewerPass, s.adminPassword
}

// Handler serves the embedded viewer, WebSocket transport and optional container
// source. When enabled, /settings/ is served by AdminHandler and protected by
// AdminPassword independently from the viewer's Password.
func (s *Server) Handler() (http.Handler, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)
	mux.HandleFunc("/api/targets", s.handleTargets)
	mux.HandleFunc("/api/targets/", s.handleTargetCover)
	mux.HandleFunc("/target/", s.handleTargetPage)
	mux.HandleFunc("/airplay/media-state", s.handleBrowserMediaState)
	mux.HandleFunc("/airplay/media/", func(w http.ResponseWriter, q *http.Request) {
		if s.cfg.BrowserMedia == nil {
			http.NotFound(w, q)
			return
		}
		s.cfg.BrowserMedia.ServeBrowserMedia(w, q)
	})
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return nil, err
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))
	root := http.NewServeMux()
	if s.cfg.AdminHandler != nil {
		admin := http.StripPrefix("/settings", s.cfg.AdminHandler)
		root.Handle("/settings/", s.adminAuth(admin))
	}
	root.Handle("/", s.viewerAuth(mux))
	return root, nil
}

func (s *Server) viewerAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, _ := s.access()
		authenticateRequest(w, r, user, password, "AnyKVM", next)
	})
}

func (s *Server) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _, password := s.access()
		if password == "" {
			if !isLoopbackRequest(r) {
				http.NotFound(w, r)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		authenticateRequest(w, r, "admin", password, "AnyKVM settings", next)
	})
}

func adminAuth(password string, next http.Handler) http.Handler {
	if password != "" {
		return basicAuth("admin", password, "AnyKVM settings", next)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackRequest(r) {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func basicAuth(expectedUser, expectedPassword, realm string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authenticateRequest(w, r, expectedUser, expectedPassword, realm, next)
	})
}

func authenticateRequest(w http.ResponseWriter, r *http.Request, expectedUser, expectedPassword, realm string, next http.Handler) {
	if expectedPassword == "" {
		next.ServeHTTP(w, r)
		return
	}
	user, password, ok := r.BasicAuth()
	if !ok || (expectedUser != "" && subtle.ConstantTimeCompare([]byte(user), []byte(expectedUser)) != 1) || subtle.ConstantTimeCompare([]byte(password), []byte(expectedPassword)) != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="`+realm+`"`)
		http.Error(w, "Authentication required", http.StatusUnauthorized)
		return
	}
	next.ServeHTTP(w, r)
}

// Start begins serving HTTP and/or HTTPS on the configured addresses.
func (s *Server) Start() error {
	handler, err := s.Handler()
	if err != nil {
		return err
	}

	// Prepare TLS and bind every address before launching any goroutines.
	// This makes startup failures visible to the caller and avoids leaving
	// HTTP running when HTTPS initialization fails.
	var cert tls.Certificate
	if s.cfg.HTTPSAddr != "" {
		if (s.cfg.TLSCertFile == "") != (s.cfg.TLSKeyFile == "") {
			return fmt.Errorf("wsmedia: TLS certificate and key must be provided together")
		}
		if s.cfg.TLSCertFile != "" {
			cert, err = tls.LoadX509KeyPair(s.cfg.TLSCertFile, s.cfg.TLSKeyFile)
		} else {
			cert, err = selfSignedCert()
		}
		if err != nil {
			return fmt.Errorf("wsmedia: TLS certificate: %w", err)
		}
	}
	type endpoint struct {
		server   *http.Server
		listener net.Listener
	}
	var endpoints []endpoint
	for _, addr := range []string{s.cfg.HTTPAddr, s.cfg.HTTPSAddr} {
		if addr == "" {
			continue
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			for _, ep := range endpoints {
				ep.listener.Close()
			}
			return fmt.Errorf("wsmedia: listen %s: %w", addr, err)
		}
		endpoints = append(endpoints, endpoint{
			server:   &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second},
			listener: netstats.WrapListener(ln),
		})
	}
	index := 0
	if s.cfg.HTTPAddr != "" {
		s.httpSrv = endpoints[index].server
		index++
	}
	if s.cfg.HTTPSAddr != "" {
		ep := &endpoints[index]
		ep.server.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		ep.listener = tls.NewListener(ep.listener, ep.server.TLSConfig)
		s.httpsSrv = ep.server
	}
	for _, ep := range endpoints {
		go func(ep endpoint) {
			if err := ep.server.Serve(ep.listener); err != nil && err != http.ErrServerClosed {
				logging.Infof("wsmedia: serve %s: %v", ep.server.Addr, err)
			}
		}(ep)
	}
	return nil
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	target, err := s.selectedTarget(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		logging.Infof("wsmedia: upgrade error: %v", err)
		return
	}
	c := &wsClient{
		conn:      conn,
		srv:       s,
		sendCh:    make(chan *outboundMessage, 8),
		jpegReady: make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
	if s.cfg.OnTargetSelected != nil {
		if err := s.cfg.OnTargetSelected(target); err != nil {
			logging.Infof("wsmedia: target selection failed: %v", err)
			_ = conn.Close()
			return
		}
	}
	// Send current metadata immediately so the client can size the canvas.
	s.mu.Lock()
	meta, videoBufferFrames := s.meta, s.httpVideoBufferFrames
	if r.TLS != nil {
		videoBufferFrames = s.httpsVideoBufferFrames
	}
	s.mu.Unlock()
	meta.VideoBufferFrames = max(1, min(8, videoBufferFrames))
	meta.MouseMode = "absolute"
	if s.cfg.MouseMode != nil && s.cfg.MouseMode() == "relative" {
		meta.MouseMode = "relative"
	}
	c.trySend(encodeMetadata(meta))
	s.addClient(c)

	go c.writeLoop()
	go func() {
		c.readLoop() // blocks until the client closes / errors
		s.removeClient(c)
	}()
}

func (s *Server) selectedTarget(r *http.Request) (int, error) {
	targets := s.webTargets()
	if len(targets) == 0 {
		return 0, nil
	}
	name := r.URL.Query().Get("target")
	if name == "" {
		return 0, nil // retain compatibility with direct /ws clients
	}
	for i, target := range targets {
		if target.Name == name {
			return i, nil
		}
	}
	return 0, fmt.Errorf("unknown target")
}

func (s *Server) handleTargets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	type item struct {
		Name  string `json:"name"`
		Cover string `json:"cover"`
	}
	settings := s.cfg.SettingsEnabled
	_, _, adminPassword := s.access()
	if adminPassword == "" && !isLoopbackRequest(r) {
		settings = false
	}
	response := struct {
		Targets  []item `json:"targets"`
		Settings bool   `json:"settings"`
	}{Settings: settings}
	for _, target := range s.webTargets() {
		response.Targets = append(response.Targets, item{
			Name:  target.Name,
			Cover: "/api/targets/" + url.PathEscape(target.Name) + "/cover",
		})
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func (s *Server) handleTargetCover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name, ok := targetNameFromEscapedPath(r, "/api/targets/", "/cover")
	if !ok {
		http.NotFound(w, r)
		return
	}
	for _, target := range s.webTargets() {
		if target.Name == name {
			if s.cfg.TargetCover == nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Cache-Control", "private, max-age=86400")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			_, _ = w.Write(s.cfg.TargetCover(target.Platform))
			return
		}
	}
	http.NotFound(w, r)
}

func (s *Server) handleTargetPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name, ok := targetNameFromEscapedPath(r, "/target/", "")
	if !ok {
		http.NotFound(w, r)
		return
	}
	for _, target := range s.webTargets() {
		if target.Name == name {
			data, err := fs.ReadFile(webFS, "web/index.html")
			if err != nil {
				http.Error(w, "load web page: "+err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(data)
			return
		}
	}
	http.NotFound(w, r)
}

func targetNameFromEscapedPath(r *http.Request, prefix, suffix string) (string, bool) {
	escaped := strings.TrimPrefix(r.URL.EscapedPath(), prefix)
	if escaped == r.URL.EscapedPath() || !strings.HasSuffix(escaped, suffix) {
		return "", false
	}
	escaped = strings.TrimSuffix(escaped, suffix)
	if escaped == "" {
		return "", false
	}
	name, err := url.PathUnescape(escaped)
	return name, err == nil
}

func (s *Server) addClient(c *wsClient) {
	if s.cfg.OnClientConnecting != nil {
		s.cfg.OnClientConnecting()
	}

	s.mu.Lock()
	s.clients[c] = struct{}{}
	n := len(s.clients)
	s.mu.Unlock()

	if s.cfg.OnVideoCodecsChanged != nil || s.cfg.OnVideoH264Changed != nil || s.cfg.OnVideoHEVCChanged != nil || s.cfg.OnVideoAV1Changed != nil {
		// Wait for hello before opening a codec-specific capture. Older clients
		// without hello still get the JPEG path after a bounded grace period.
		time.AfterFunc(time.Second, func() { s.activateClient(c) })
	} else {
		s.activateClient(c)
	}
	logging.Infof("wsmedia: client connected (total: %d)", n)
}

func (s *Server) activateClient(c *wsClient) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.mu.Lock()
	if _, present := s.clients[c]; !present || c.active {
		s.mu.Unlock()
		return
	}
	select {
	case <-c.done:
		s.mu.Unlock()
		return
	default:
	}
	c.active = true
	first := s.activeClients == 0
	s.activeClients++
	s.mu.Unlock()
	if first && s.cfg.OnClientConnected != nil {
		s.cfg.OnClientConnected()
	}
}

func (s *Server) removeClient(c *wsClient) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.mu.Lock()
	if _, ok := s.clients[c]; !ok {
		s.mu.Unlock()
		return
	}
	delete(s.clients, c)
	if c.active {
		s.activeClients--
	}
	lastActive := c.active && s.activeClients == 0
	s.mu.Unlock()

	s.releaseAll(c)
	c.close()
	if lastActive && s.cfg.OnClientDisconnected != nil {
		s.cfg.OnClientDisconnected()
	}
	logging.Infof("wsmedia: client disconnected")
}

// --- bridge.FrameServer ---

func (s *Server) PushFrame(f bridge.Frame) {
	s.PushFrameWithStop(f, nil)
}

// PushFrameWithStop lets a capture restart interrupt a congested stream.
func (s *Server) PushFrameWithStop(f bridge.Frame, stop <-chan struct{}) {
	var kind byte
	switch f.Format {
	case capture.FormatJPEG:
		kind = msgJPEG
	case capture.FormatH264:
		kind = msgH264
	case capture.FormatHEVC:
		kind = msgHEVC
	case capture.FormatAV1:
		// Validation and WebCodecs recovery flag; payload stays in its source buffer.
		encoded, err := encodeAV1Header(f.Data)
		if err != nil {
			logging.Infof("wsmedia: %v", err)
			return
		}
		s.broadcastVideo(f, encoded, true, stop)
		return
	default:
		return
	}
	if len(f.Data) == 0 {
		return
	}
	s.broadcastVideo(f, []byte{kind}, kind != msgJPEG, stop)
}

// --- input from browser ---

// handleInput dispatches one client->server message to the HID device.
func (s *Server) handleInput(c *wsClient, data []byte) {
	if len(data) == 0 {
		return
	}
	switch data[0] {
	case msgInputKeyboard:
		s.handleKeyboard(c, data)
	case msgInputMouse, msgInputMouseRelative:
		s.handlePointer(c, data)
	case msgHello:
		if s.handleHello(data[1:], c) {
			s.activateClient(c)
		}
	case msgKeyframeReq:
		if s.cfg.OnVideoKeyframeRequested != nil && time.Since(c.lastKeyframeRequest) >= 500*time.Millisecond {
			c.lastKeyframeRequest = time.Now()
			s.cfg.OnVideoKeyframeRequested()
		}
	case msgVideoDiagnostics:
		if time.Since(c.lastVideoDiagnostics) >= time.Second {
			if s.logVideoDiagnostics(data[1:]) {
				c.lastVideoDiagnostics = time.Now()
			}
		}
	}
}

func (s *Server) logVideoDiagnostics(body []byte) bool {
	if len(body) > 4096 {
		return false
	}
	var stats struct {
		ReceivedFPS            float64 `json:"receivedFps"`
		DecodedFPS             float64 `json:"decodedFps"`
		DrawnFPS               float64 `json:"drawnFps"`
		Discarded              int     `json:"discarded"`
		Queue                  int     `json:"queue"`
		QueueMax               int     `json:"queueMax"`
		PendingOutput          int     `json:"pendingOutput"`
		PendingMax             int     `json:"pendingMax"`
		KeyRequests            int     `json:"keyRequests"`
		UnmatchedOutput        int     `json:"unmatchedOutput"`
		DecodeAvgMS            float64 `json:"decodeAvgMs"`
		DrawAvgMS              float64 `json:"drawAvgMs"`
		DrawMaxMS              float64 `json:"drawMaxMs"`
		RefreshFPS             float64 `json:"refreshFps"`
		RefreshGapMS           float64 `json:"refreshGapMaxMs"`
		SourceGapMS            float64 `json:"sourceGapMaxMs"`
		Visible                string  `json:"visible"`
		InputGapMS             float64 `json:"inputGapMaxMs"`
		OutputGapMS            float64 `json:"outputGapMaxMs"`
		BufferMS               float64 `json:"presentationTargetBufferMs"`
		DisplayWaitMS          float64 `json:"presentationWaitMaxMs"`
		DisplayQueue           int     `json:"presentationQueue"`
		DisplayQueueMax        int     `json:"presentationQueueMax"`
		PresentationMode       string  `json:"presentationMode"`
		Superseded             int     `json:"presentationSuperseded"`
		OverflowDrops          int     `json:"presentationOverflowDrops"`
		CatchUpDrops           int     `json:"presentationCatchUpDrops"`
		DecodedBurstMax        int     `json:"decodedBurstMax"`
		WaitedRefreshes        int     `json:"presentationWaitedRefreshes"`
		Rebases                int     `json:"presentationRebases"`
		IntervalSamples        int     `json:"presentationIntervalSamples"`
		IntervalErrorMeanMS    float64 `json:"presentationIntervalErrorMeanMs"`
		IntervalErrorAbsMeanMS float64 `json:"presentationIntervalErrorAbsMeanMs"`
		IntervalErrorAbsMaxMS  float64 `json:"presentationIntervalErrorAbsMaxMs"`
	}
	if err := json.Unmarshal(body, &stats); err != nil {
		return false
	}
	logging.Debugf("wsmedia: browser video receive=%.1f decode=%.1f draw=%.1ffps decode_avg=%.1fms queue=%d/%d pending_output=%d/%d discarded=%d recovery=%d unmatched=%d gaps=%.1f/%.1fms buffer=%.1fms display_wait=%.1fms display_queue=%d/%d superseded=%d rebases=%d presentation=%q raf=%.1ffps raf_gap=%.1fms draw_ms=%.1f/%.1f source_gap=%.1fms visible=%q display_drops=%d/%d burst=%d pts_waits=%d interval_delta_mean_abs_max=%.1f/%.1f/%.1fms interval_samples=%d", stats.ReceivedFPS, stats.DecodedFPS, stats.DrawnFPS, stats.DecodeAvgMS, stats.Queue, stats.QueueMax, stats.PendingOutput, stats.PendingMax, stats.Discarded, stats.KeyRequests, stats.UnmatchedOutput, stats.InputGapMS, stats.OutputGapMS, stats.BufferMS, stats.DisplayWaitMS, stats.DisplayQueue, stats.DisplayQueueMax, stats.Superseded, stats.Rebases, stats.PresentationMode, stats.RefreshFPS, stats.RefreshGapMS, stats.DrawAvgMS, stats.DrawMaxMS, stats.SourceGapMS, stats.Visible, stats.OverflowDrops, stats.CatchUpDrops, stats.DecodedBurstMax, stats.WaitedRefreshes, stats.IntervalErrorMeanMS, stats.IntervalErrorAbsMeanMS, stats.IntervalErrorAbsMaxMS, stats.IntervalSamples)
	return true
}

// handleKeyboard: [0x80][down u8][code ASCII...]. The physical key code is
// mapped to a USB HID usage and injected. Repeats are suppressed and the pressed
// set is tracked so releaseAll can clear stuck keys on disconnect.
func (s *Server) handleKeyboard(c *wsClient, data []byte) {
	if s.cfg.HID == nil || len(data) < 3 {
		return
	}
	down := data[1] != 0
	code := string(data[2:])
	usage, ok := hidForCode(code)
	if !ok {
		return
	}
	if c.pressed == nil {
		c.pressed = make(map[uint8]bool)
	}
	if down {
		if !c.pressed[usage] {
			c.pressed[usage] = true
			s.cfg.HID.KeyDown(usage)
		}
	} else {
		delete(c.pressed, usage)
		s.cfg.HID.KeyRelease(usage)
	}
}

// handlePointer: [0x81][buttons u8][x u16 BE][y u16 BE]. Mirrors the VNC pointer
// handler: 0x87 uses signed relative deltas, 0x81 absolute coords scaled to the HID 0..32767 range, per-button edges,
// and wheel steps on the press edge.
func (s *Server) handlePointer(c *wsClient, data []byte) {
	if s.cfg.HID == nil || len(data) < 6 {
		return
	}
	started := time.Now()
	defer c.recordPointerInput(started)
	buttons := data[1]
	x := int(data[2])<<8 | int(data[3])
	y := int(data[4])<<8 | int(data[5])

	if data[0] == msgInputMouseRelative {
		dx, dy := int(int16(binary.BigEndian.Uint16(data[2:4]))), int(int16(binary.BigEndian.Uint16(data[4:6])))
		if dx != 0 || dy != 0 {
			s.cfg.HID.MouseMoveRel(dx, dy)
		}
		c.positionSet = false
	} else {
		s.mu.Lock()
		w, h := s.meta.Width, s.meta.Height
		s.mu.Unlock()
		if (w > 0 && h > 0) && (!c.positionSet || x != c.lastX || y != c.lastY) {
			s.cfg.HID.MouseMove(pointerCoordinate(x, w), pointerCoordinate(y, h))
			c.positionSet = true
			c.lastX, c.lastY = x, y
		}
	}

	for b := 0; b < 3; b++ {
		mask := uint8(1) << b
		was := c.lastButtons&mask != 0
		now := buttons&mask != 0
		if was != now {
			s.cfg.HID.MouseButton(vncBtnToHID[b], now)
		}
	}
	if buttons&0x08 != 0 && c.lastButtons&0x08 == 0 {
		s.cfg.HID.MouseWheel(1)
	}
	if buttons&0x10 != 0 && c.lastButtons&0x10 == 0 {
		s.cfg.HID.MouseWheel(-1)
	}
	c.lastButtons = buttons
}

// Measure input independently of video output. The read loop owns these fields;
// no timer or capture lock can delay forwarding a mouse event.
func (c *wsClient) recordPointerInput(started time.Time) {
	stats := &c.pointerStats
	if stats.started.IsZero() {
		stats.started = started
	}
	if !stats.last.IsZero() {
		stats.maxGap = max(stats.maxGap, started.Sub(stats.last))
	}
	stats.last = started
	stats.events++
	stats.maxHID = max(stats.maxHID, time.Since(started))
	if elapsed := started.Sub(stats.started); elapsed >= 5*time.Second {
		logging.Debugf("wsmedia: pointer input %.1f events/s events=%d max_gap=%.1fms max_hid=%.1fms (gaps include idle time)", float64(stats.events)/elapsed.Seconds(), stats.events, float64(stats.maxGap)/float64(time.Millisecond), float64(stats.maxHID)/float64(time.Millisecond))
		*stats = pointerInputStats{started: started}
	}
}

// pointerCoordinate maps framebuffer endpoints to the full HID range.
// Clamp stale coordinates received just after the captured display shrinks.
func pointerCoordinate(pixel, size int) int {
	if size <= 1 {
		return 0
	}
	return min(max(pixel, 0), size-1) * 32767 / (size - 1)
}

// releaseAll releases any keys/buttons the client left pressed (called on
// disconnect) to avoid stuck input on the remote host.
func (s *Server) releaseAll(c *wsClient) {
	if s.cfg.HID == nil {
		return
	}
	for usage := range c.pressed {
		s.cfg.HID.KeyRelease(usage)
	}
	c.pressed = nil
	for b := 0; b < 3; b++ {
		if c.lastButtons&(uint8(1)<<b) != 0 {
			s.cfg.HID.MouseButton(vncBtnToHID[b], false)
		}
	}
	c.lastButtons = 0
}

// handleHello parses the client capability JSON and reports H.264 / HEVC support
// so the capture layer can pick its output codec.
func (s *Server) handleHello(body []byte, clients ...*wsClient) bool {
	if len(body) > 4096 {
		return false
	}
	var caps struct {
		H264         bool             `json:"h264"`
		HEVC         bool             `json:"hevc"`
		AV1          bool             `json:"av1"`
		RawPointer   bool             `json:"rawPointer"`
		AudioCodecs  []string         `json:"audioCodecs"`
		AudioFormats *[]audioMetadata `json:"audioFormats"`
	}
	if err := json.Unmarshal(body, &caps); err != nil {
		return false
	}
	s.codecsMu.RLock()
	caps.H264 = caps.H264 && s.enableH264
	caps.HEVC = caps.HEVC && s.enableHEVC
	caps.AV1 = caps.AV1 && s.enableAV1
	s.codecsMu.RUnlock()
	if len(clients) > 0 {
		s.mu.Lock()
		clients[0].audioCodecs = make(map[string]bool)
		clients[0].audioFormats = nil
		if caps.AudioFormats != nil {
			clients[0].audioFormats = make(map[string]bool)
			for _, format := range *caps.AudioFormats {
				if knownAudioFormat(format) {
					clients[0].audioFormats[audioFormatKey(format)] = true
				}
			}
		}
		for _, codec := range caps.AudioCodecs {
			if codec == "mp4a.40.39" || codec == "mp4a.40.2" {
				clients[0].audioCodecs[codec] = true
			}
		}
		s.mu.Unlock()
	}
	logging.Infof("wsmedia: client caps h264=%v hevc=%v av1=%v raw_pointer=%v", caps.H264, caps.HEVC, caps.AV1, caps.RawPointer)
	if s.cfg.OnVideoCodecsChanged != nil {
		s.cfg.OnVideoCodecsChanged(caps.H264, caps.HEVC, caps.AV1)
		return true
	}
	if s.cfg.OnVideoAV1Changed != nil {
		s.cfg.OnVideoAV1Changed(caps.AV1)
	}
	if s.cfg.OnVideoH264Changed != nil {
		s.cfg.OnVideoH264Changed(caps.H264)
	}
	if s.cfg.OnVideoHEVCChanged != nil {
		s.cfg.OnVideoHEVCChanged(caps.HEVC)
	}
	return true
}

func (s *Server) UpdateFramebufferSize(width, height int) {
	s.mu.Lock()
	s.meta.Width = width
	s.meta.Height = height
	meta := s.meta
	s.mu.Unlock()
	s.broadcast(encodeMetadata(meta))
}

func (s *Server) CloseClients() {
	s.mu.Lock()
	clients := make([]*wsClient, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()
	for _, c := range clients {
		c.close()
	}
}

// --- bridge.AudioSink ---

// SetAudioAvailable reports actual delivery, independently of user mute state.
func (s *Server) SetAudioAvailable(available bool) {
	s.mu.Lock()
	if s.meta.AudioAvailable == available {
		s.mu.Unlock()
		return
	}
	s.meta.AudioAvailable = available
	meta := s.meta
	s.mu.Unlock()
	s.broadcast(encodeMetadata(meta))
}

func (s *Server) PushAudio(a capture.AudioFrame) {
	if len(a.Encoded) > 0 {
		if s.SupportsEncodedAudio(a) {
			s.broadcast(encodeAudio(a))
		}
		return
	}
	if len(a.PCM) == 0 {
		return
	}
	msg := make([]byte, 1+len(a.PCM))
	msg[0] = msgPCM
	copy(msg[1:], a.PCM)
	s.broadcast(msg)
}

func (s *Server) broadcast(msg []byte) {
	s.mu.Lock()
	clients := make([]*wsClient, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()

	for _, c := range clients {
		c.trySend(msg)
	}
}

// broadcastH264 is like broadcast but never drops — H.264 is stateful and a
// single missing chunk corrupts the stream. The channel send blocks under
// backpressure, throttling the capture encoder instead.
func (s *Server) broadcastH264(msg []byte, stop <-chan struct{}) {
	s.mu.Lock()
	clients := make([]*wsClient, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()

	for _, c := range clients {
		c.enqueue(&outboundMessage{data: msg}, true, stop)
	}
}

// broadcastVideo acquires one stable backing buffer for all selected clients.
func (s *Server) broadcastVideo(f bridge.Frame, header []byte, ordered bool, stop <-chan struct{}) {
	s.mu.Lock()
	var geometry []byte
	bufferH264 := f.Format == capture.FormatH264 && f.BufferH264Presentation
	policyChanged := s.meta.BufferH264 != bufferH264
	s.meta.BufferH264 = bufferH264
	rotation := f.Rotation
	if rotation != 90 && rotation != 180 && rotation != 270 {
		rotation = 0
	}
	if f.Width > 0 && f.Height > 0 && (s.meta.Width != f.Width || s.meta.Height != f.Height || s.meta.Rotation != rotation) {
		s.meta.Width, s.meta.Height, s.meta.Rotation = f.Width, f.Height, rotation
		geometry = encodeMetadata(s.meta)
	}
	if policyChanged && geometry == nil {
		geometry = encodeMetadata(s.meta)
	}
	clients := make([]*wsClient, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()
	if len(clients) == 0 {
		return
	}
	owner := framebuf.Acquire(f.Data, f.Buffer)
	defer owner.Release()
	payload := f.Data
	if f.Buffer == nil {
		payload = owner.Bytes()
	}
	if len(header) == 1 && header[0] == msgH264 {
		stamp := f.CapturedAt.UnixMicro()
		// Sources without a usable PTS still carry a timestamp. Keep its value
		// exactly representable in browser Number values.
		if f.CapturedAt.IsZero() || stamp <= 0 || stamp > 1<<53-1 {
			stamp = time.Now().UnixMicro()
		}
		header = make([]byte, 9)
		header[0] = msgH264
		binary.LittleEndian.PutUint64(header[1:], uint64(stamp))
	}
	for _, c := range clients {
		if geometry != nil {
			// Orientation must precede the corresponding picture, even when the
			// output queue is full. Keep encoded dimensions and bytes untouched.
			c.enqueue(&outboundMessage{data: geometry}, true, stop)
		}
		c.enqueue(&outboundMessage{timing: f.Timing, header: header, data: payload, buffer: owner.Retain()}, ordered, stop)
	}
}

func (s *Server) Close() error {
	s.CloseClients()
	var err error
	if s.httpSrv != nil {
		if e := s.httpSrv.Close(); e != nil {
			err = e
		}
	}
	if s.httpsSrv != nil {
		if e := s.httpsSrv.Close(); e != nil {
			err = e
		}
	}
	return err
}

func encodeMetadata(m metadata) []byte {
	body, _ := json.Marshal(m)
	msg := make([]byte, 1+len(body))
	msg[0] = msgMetadata
	copy(msg[1:], body)
	return msg
}

// AV1 message: type, recovery flag, codec-string length, UTF-8 codec string,
// followed by one complete low-overhead OBU temporal unit. No IVF wrapper.
func encodeAV1Header(data []byte) ([]byte, error) {
	info, err := av1.Inspect(data)
	if err != nil {
		return nil, err
	}
	msg := make([]byte, 3+len(info.Codec))
	msg[0] = msgAV1
	if info.Recovery {
		msg[1] = 1
	}
	msg[2] = byte(len(info.Codec))
	copy(msg[3:], info.Codec)
	return msg, nil
}

func encodeAV1Frame(data []byte) ([]byte, error) {
	h, err := encodeAV1Header(data)
	if err != nil {
		return nil, err
	}
	return append(h, data...), nil
}
