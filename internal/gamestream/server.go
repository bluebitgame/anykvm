// Package gamestream implements a single-client GameStream host.
package gamestream

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"anykvm/internal/gamestream/native"
)

type PairingRequest struct{ ClientName, Address string }

type Config struct {
	H264Profile                  string // Native VideoToolbox: empty/constrained_baseline, baseline, main or high.
	DisableLowLatencyRateControl bool   // Native VideoToolbox H.264 only; zero keeps low-latency rate control enabled.
	EnableAV1                    bool
	SupportsAV1                  func(machineIndex int) bool
	EnableHEVC                   bool // explicit opt-in; hardware support alone must not enable HEVC
	SupportsHEVC                 func(machineIndex int) bool
	VideoPacingMbps              int // UDP burst ceiling; 0 uses 800 Mbps, independent of encoder bitrate.
	HTTPAddr                     string
	HTTPSAddr                    string
	StateFile                    string
	Name                         string
	Machines                     []string
	// MachinePlatforms selects cover art by local screen GOOS, "android", or "ios".
	// Recognized Linux distributions use "linux-<ID>".
	// Omitted or unknown entries use generic art.
	MachinePlatforms []string
	// MachineAudio enables audio encoding for each configured target; omitted entries disable it.
	MachineAudio []bool
	// ClientOS overrides client OS by paired certificate SHA-256 fingerprint.
	ClientOS map[string]string
	// WrapClientHID creates an optional per-session input mapper. Nil (or a nil
	// result) keeps HID unchanged. The wrapper must forward mouse events and
	// handle matched key releases, including disconnect cleanup. It must not
	// close the shared HID. A fresh wrapper is created on launch and resume.
	WrapClientHID func(ClientInfo, bridge.HIDSender) bridge.HIDSender
	// RequestPIN asks the local operator for the PIN displayed by the client.
	// It must honor ctx cancellation. Nil disables new pairings.
	RequestPIN      func(context.Context, PairingRequest) (string, error)
	OnStreamStart   func(machineIndex int, video capture.VideoStreamOptions) error
	OnVideoRestart  func() error
	OnVideoKeyframe func() (bool, error)
	// Nil allows legacy restart recovery. False waits for source recovery frames.
	OnVideoRestartAllowed func() bool
	// Also allows initial placeholders when capture restart is forbidden.
	// A local encoder can transition from a placeholder to passive raw capture
	// without reopening the sender. Nil retains the restart-based default.
	OnVideoPlaceholderAllowed func() bool
	OnStreamStop              func()
	HID                       bridge.HIDSender
}

type app struct {
	Title string `xml:"AppTitle"`
	ID    int    `xml:"ID"`
	HDR   int    `xml:"IsHdrSupported"`
}

type field struct {
	XMLName xml.Name
	Value   string `xml:",chardata"`
}

type response struct {
	XMLName xml.Name `xml:"root"`
	Code    int      `xml:"status_code,attr"`
	Message string   `xml:"status_message,attr,omitempty"`
	Fields  []field  `xml:",any"`
	Apps    []app    `xml:"App,omitempty"`
}

func reply(w http.ResponseWriter, code int, message string, values ...string) {
	if code >= 400 {
		log.Printf("gamestream: API rejected request (%d): %s", code, message)
	}
	r := response{Code: code, Message: message}
	for i := 0; i+1 < len(values); i += 2 {
		r.Fields = append(r.Fields, field{xml.Name{Local: values[i]}, values[i+1]})
	}
	writeResponse(w, r)
}

func writeResponse(w http.ResponseWriter, r response) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// GameStream carries status in XML, including pairing errors.
	w.Write([]byte(xml.Header))
	xml.NewEncoder(w).Encode(r)
}

type Server struct {
	cfg                         Config
	apps                        []app
	cert                        tls.Certificate
	mu                          sync.Mutex
	state                       identity
	pairing                     *pairSession // one operator-approved pairing at a time
	httpPort, httpsPort         int
	lifeMu                      sync.Mutex
	servers                     []*http.Server
	closed                      bool
	ctx                         context.Context
	cancel                      context.CancelFunc
	launchMu                    sync.Mutex
	streamMu                    sync.Mutex
	session                     *streamSession
	rtspListener                net.Listener
	videoSocket, audioSocket    *net.UDPConn
	control                     *native.Host
	rtspPort, controlPort       int
	streamWorkers, rtspHandlers sync.WaitGroup
}

func NewServer(cfg Config) (*Server, error) {
	clientOS, err := ParseClientOSOverrides(cfg.ClientOS)
	if err != nil {
		return nil, err
	}
	cfg.ClientOS = clientOS
	if cfg.VideoPacingMbps < 0 || cfg.VideoPacingMbps > 10000 {
		return nil, errors.New("video pacing must be 0 (default) or 1..10000 Mbps")
	}
	if cfg.StateFile == "" {
		return nil, errors.New("GameStream state file is required")
	}
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = ":47989"
	}
	if cfg.HTTPSAddr == "" {
		cfg.HTTPSAddr = ":47984"
	}
	if cfg.Name == "" {
		cfg.Name = "AnyKVM"
	}
	if len(cfg.Machines) == 0 {
		return nil, errors.New("GameStream needs at least one target")
	}
	apps := make([]app, 0, len(cfg.Machines))
	ids := make(map[int]bool)
	for _, name := range cfg.Machines {
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("GameStream targets need nonempty names")
		}
		// Stable across reordering; fail rather than silently route a collision.
		h := sha256.Sum256([]byte(name))
		id := int(binary.BigEndian.Uint32(h[:4]) & 0x7fffffff)
		if id == 0 {
			id = 1
		}
		if ids[id] {
			return nil, fmt.Errorf("duplicate GameStream target name or app ID: %q", name)
		}
		ids[id] = true
		apps = append(apps, app{Title: name, ID: id})
	}
	state, cert, err := loadIdentity(cfg.StateFile)
	if err != nil {
		return nil, fmt.Errorf("GameStream identity: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{cfg: cfg, apps: apps, state: state, cert: cert, ctx: ctx, cancel: cancel}, nil
}

// Start binds both listeners before serving, so a port conflict leaves no
// partially running host. A Server can be started once and closed repeatedly.
func (s *Server) Start() error {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if s.closed || len(s.servers) != 0 {
		return errors.New("GameStream server already started or closed")
	}
	plain, err := net.Listen("tcp", s.cfg.HTTPAddr)
	if err != nil {
		return err
	}
	secure, err := net.Listen("tcp", s.cfg.HTTPSAddr)
	if err != nil {
		plain.Close()
		return err
	}
	s.httpPort = plain.Addr().(*net.TCPAddr).Port
	s.httpsPort = secure.Addr().(*net.TCPAddr).Port
	if s.cfg.OnStreamStart != nil {
		ip, baseText, _ := net.SplitHostPort(s.cfg.HTTPAddr)
		base, _ := strconv.Atoi(baseText)
		if err := s.bindStreaming(ip, base); err != nil {
			plain.Close()
			secure.Close()
			return fmt.Errorf("GameStream transport: %w", err)
		}
		s.serveStreaming()
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{s.cert}, MinVersion: tls.VersionTLS12,
		// TLS verifies proof of private-key possession. Authorization below
		// uses exact paired certificates, not public CAs or caller uniqueid.
		ClientAuth: tls.RequestClientCert,
	}
	for _, listener := range []net.Listener{plain, tls.NewListener(secure, tlsCfg)} {
		srv := &http.Server{
			Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
			WriteTimeout: 150 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 * 1024,
			BaseContext: func(net.Listener) context.Context { return s.ctx },
		}
		s.servers = append(s.servers, srv)
		go func(l net.Listener, server *http.Server) {
			if err := server.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("gamestream: listener stopped: %v", err)
			}
		}(listener, srv)
	}
	return nil
}

func (s *Server) Close() {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	s.closed = true
	s.launchMu.Lock()
	s.cancel()
	s.CloseClients()
	s.launchMu.Unlock()
	s.closeStreamingSockets()
	for _, server := range s.servers {
		server.Close()
	}
	s.mu.Lock()
	s.pairing = nil
	s.mu.Unlock()
	s.streamWorkers.Wait()
	s.rtspHandlers.Wait()
}

func (s *Server) authorized(r *http.Request) bool {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.state.Clients[fingerprint(r.TLS.PeerCertificates[0])]
	return ok
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		reply(w, 405, "GET required")
		return
	}
	if len(r.URL.RawQuery) > 12*1024 {
		reply(w, 400, "Query too large")
		return
	}
	args, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		reply(w, 400, "Invalid query")
		return
	}
	// Reject ambiguous duplicate values in authentication and pairing requests.
	for _, v := range args {
		if len(v) != 1 {
			err = errors.New("duplicate parameter")
		}
	}
	if err != nil {
		reply(w, 400, "Duplicate parameters")
		return
	}
	auth := s.authorized(r)
	if r.TLS != nil && !auth {
		reply(w, 401, "Client certificate is not paired")
		return
	}
	switch r.URL.Path {
	case "/serverinfo":
		s.serverInfo(w, r, auth)
	case "/pair":
		s.pair(w, r, auth)
	case "/unpair":
		s.unpair(w, r, auth)
	default:
		if !auth {
			reply(w, 401, "Paired HTTPS client required")
			return
		}
		switch r.URL.Path {
		case "/applist":
			writeResponse(w, response{Code: 200, Apps: s.apps})
		case "/appasset":
			s.appAsset(w, r)
		case "/launch", "/resume":
			s.launch(w, r)
		case "/cancel":
			s.cancelStream(w, r)
		default:
			reply(w, 404, fmt.Sprintf("Unknown endpoint %q", r.URL.Path))
		}
	}
}

func (s *Server) serverInfo(w http.ResponseWriter, r *http.Request, auth bool) {
	ip := "127.0.0.1"
	if addr, ok := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr); ok && addr.IP.To4() != nil {
		ip = addr.IP.String()
	}
	paired := "0"
	if auth {
		paired = "1"
	}
	game, state := "0", "SUNSHINE_SERVER_FREE"
	if x := s.currentSession(); x != nil {
		game, state = strconv.Itoa(x.appID), "SUNSHINE_SERVER_BUSY"
	}
	codecMask, hevcPixels := 1, "0"
	for i := range s.cfg.Machines {
		if s.supportsHEVC(i) {
			codecMask |= 0x100
			hevcPixels = "1869449984"
		}
		if s.supportsAV1(i) {
			codecMask |= 0x10000
		}
	}
	reply(w, 200, "", "hostname", s.cfg.Name, "appversion", "7.1.431.0", "GfeVersion", "3.23.0.74",
		"uniqueid", s.state.ID, "HttpsPort", strconv.Itoa(s.httpsPort), "ExternalPort", strconv.Itoa(s.httpPort),
		"LocalIP", ip, "mac", "00:00:00:00:00:00", "PairStatus", paired,
		"currentgame", game, "state", state, "MaxLumaPixelsHEVC", hevcPixels, "ServerCodecModeSupport", strconv.Itoa(codecMask))
}

func (s *Server) supportsHEVC(machine int) bool {
	return s != nil && s.cfg.EnableHEVC && s.cfg.SupportsHEVC != nil && s.cfg.SupportsHEVC(machine)
}

func (s *Server) supportsAV1(machine int) bool {
	return s != nil && s.cfg.EnableAV1 && s.cfg.SupportsAV1 != nil && s.cfg.SupportsAV1(machine)
}
