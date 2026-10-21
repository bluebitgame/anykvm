package gamestream

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"anykvm/internal/gamestream/native"
)

type streamOptions struct {
	Codec                             string
	Width, Height, FPS, Bitrate       int
	PacketSize, MinFEC, AudioDuration int
	Encryption, Features              int
}

type streamSession struct {
	server                               *Server
	owner, ip, id, ping                  string
	appID, machine                       int
	key                                  []byte
	keyID, connectData                   uint32
	rtspEncrypted                        bool
	gcm                                  cipher.AEAD
	ctx                                  context.Context
	cancel                               context.CancelFunc
	done                                 chan struct{}
	stopOnce                             sync.Once
	startMu                              sync.Mutex // serializes PLAY and teardown callbacks
	mu                                   sync.Mutex
	options                              streamOptions
	announced, started                   bool
	setup                                map[string]bool
	videoPeer, audioPeer                 *net.UDPAddr
	rtspIn, rtspOut                      uint32
	framePushMu                          sync.Mutex
	frames                               chan bridge.Frame
	audio                                chan capture.AudioFrame
	keyframe                             chan struct{}
	workers                              sync.WaitGroup
	lastControl                          atomic.Int64
	acceptFrames                         atomic.Bool
	input                                inputState
	client                               ClientInfo // immutable snapshot, also supplied to the input mapper
	loggedVideoCodec, loggedVideoEncoder string     // video worker only; survives encoder rebuilds
}

func (x *streamSession) logVideoEncoder(codec, encoder string) {
	if codec == "" {
		codec = "h264"
	}
	if codec != x.loggedVideoCodec || encoder != x.loggedVideoEncoder {
		log.Printf("gamestream: video codec=%s encoder=%q", codec, encoder)
		x.loggedVideoCodec, x.loggedVideoEncoder = codec, encoder
	}
}

func (s *Server) currentSession() *streamSession {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	return s.session
}

func (s *Server) launch(w http.ResponseWriter, r *http.Request) {
	s.launchMu.Lock()
	defer s.launchMu.Unlock()
	if s.ctx.Err() != nil {
		reply(w, 503, "Server is shutting down")
		return
	}
	// Authorization may have changed while this request waited for launchMu.
	if !s.authorized(r) {
		reply(w, 401, "Client is no longer paired")
		return
	}
	if s.cfg.OnStreamStart == nil {
		reply(w, 503, "GameStream streaming backend is not configured", "gamesession", "0")
		return
	}
	q := r.URL.Query()
	owner := fingerprint(r.TLS.PeerCertificates[0])
	old := s.currentSession()
	appID, err := strconv.Atoi(q.Get("appid"))
	if r.URL.Path == "/resume" {
		if old == nil || old.owner != owner {
			reply(w, 409, "No session owned by this client to resume")
			return
		}
		appID, err = old.appID, nil
	} else if old != nil {
		reply(w, 409, "Another GameStream session is active", "gamesession", "0")
		return
	}
	index := -1
	for i, a := range s.apps {
		if a.ID == appID {
			index = i
		}
	}
	if err != nil || index < 0 {
		reply(w, 404, "Unknown target app ID", "gamesession", "0")
		return
	}
	key, err := hex.DecodeString(q.Get("rikey"))
	if err != nil || len(key) != 16 {
		reply(w, 400, "rikey must contain 16 bytes")
		return
	}
	keyID, err := parseKeyID(q.Get("rikeyid"))
	if err != nil {
		reply(w, 400, "Invalid rikeyid")
		return
	}
	if old != nil {
		if string(old.key) == string(key) {
			reply(w, 400, "Resume requires a fresh session key")
			return
		}
		old.stop("resumed by owner")
		<-old.done
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	random := make([]byte, 16)
	if _, err = rand.Read(random); err != nil {
		reply(w, 500, "Session randomness failed")
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	x := &streamSession{server: s, owner: owner, ip: peerAddress(r), id: hex.EncodeToString(random), ping: hex.EncodeToString(random[:8]), appID: appID, machine: index, key: key, keyID: uint32(keyID), connectData: binary.LittleEndian.Uint32(random[8:12]), gcm: gcm, ctx: ctx, cancel: cancel, done: make(chan struct{}), setup: make(map[string]bool), frames: make(chan bridge.Frame, 1), audio: make(chan capture.AudioFrame, 16), keyframe: make(chan struct{}, 1)}
	x.input.hid = s.cfg.HID
	x.client = s.sessionClientInfo(r)
	if s.cfg.WrapClientHID != nil {
		if mapped := s.cfg.WrapClientHID(x.client, s.cfg.HID); mapped != nil {
			x.input.hid = mapped
		}
	}
	core, _ := strconv.Atoi(q.Get("corever"))
	x.rtspEncrypted = core >= 1
	s.streamMu.Lock()
	s.session = x
	s.streamMu.Unlock()
	go func() {
		select {
		case <-time.After(30 * time.Second):
			x.mu.Lock()
			started := x.started
			x.mu.Unlock()
			if !started {
				x.stop("RTSP startup timed out")
			}
		case <-ctx.Done():
		}
	}()
	addr := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	ip, _, _ := net.SplitHostPort(addr.String())
	scheme := "rtsp"
	if x.rtspEncrypted {
		scheme = "rtspenc"
	}
	field := "gamesession"
	if r.URL.Path == "/resume" {
		field = "resume"
	}
	log.Printf("gamestream: launching target %q for %s client_os=%s os_source=%s apple_shortcuts=%t client_cert=%s User-Agent=%q", s.apps[index].Title, x.ip, x.client.OS, x.client.OSSource, x.client.UsesAppleShortcuts(), x.client.CertificateFingerprint, x.client.UserAgent)
	reply(w, 200, "", field, "1", "sessionUrl0", scheme+"://"+net.JoinHostPort(ip, strconv.Itoa(s.rtspPort)))
}

// GameStream clients may serialize this IV prefix as an int32. Accept its signed
// decimal representation as well as uint32, preserving all 32 bits.
func parseKeyID(s string) (uint32, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < -1<<31 || n > 1<<32-1 {
		return 0, errors.New("invalid 32-bit key ID")
	}
	return uint32(n), nil
}

func (s *Server) cancelStream(w http.ResponseWriter, r *http.Request) {
	s.launchMu.Lock()
	defer s.launchMu.Unlock()
	if x := s.currentSession(); x != nil {
		if x.owner != fingerprint(r.TLS.PeerCertificates[0]) {
			reply(w, 403, "Session belongs to another client")
			return
		}
		x.stop("cancelled by owner")
		<-x.done
	}
	reply(w, 200, "", "cancel", "1")
}

func (x *streamSession) stop(reason string) {
	x.stopOnce.Do(func() {
		log.Printf("gamestream: stopping session: %s", reason)
		x.cancel()
		go func() {
			x.startMu.Lock()
			defer x.startMu.Unlock()
			x.input.release()
			x.workers.Wait()
			x.drainFrames()
			x.mu.Lock()
			started := x.started
			x.mu.Unlock()
			if started && x.server.cfg.OnStreamStop != nil {
				x.server.cfg.OnStreamStop()
			}
			x.server.streamMu.Lock()
			if x.server.session == x {
				x.server.session = nil
			}
			x.server.streamMu.Unlock()
			close(x.done)
		}()
	})
}

func (x *streamSession) play() error {
	x.startMu.Lock()
	defer x.startMu.Unlock()
	if err := x.ctx.Err(); err != nil {
		return err
	}
	x.mu.Lock()
	ready := x.announced && x.setup["audio"] && x.setup["video"] && x.setup["control"]
	started := x.started
	x.mu.Unlock()
	if started {
		return nil
	}
	if !ready {
		return errors.New("SETUP and ANNOUNCE required before PLAY")
	}
	// Reserve lifecycle state before calling Bridge; teardown is serialized by startMu.
	x.mu.Lock()
	x.started = true
	x.mu.Unlock()
	x.input.mu.Lock()
	x.input.active = true
	x.input.mu.Unlock()
	if err := x.server.cfg.OnStreamStart(x.machine, capture.VideoStreamOptions{H264Profile: x.server.cfg.H264Profile, DisableLowLatencyRateControl: x.server.cfg.DisableLowLatencyRateControl, Codec: x.options.Codec, Width: x.options.Width, Height: x.options.Height, FPS: x.options.FPS, Bitrate: x.options.Bitrate}); err != nil {
		x.stop("capture start failed")
		return err
	}
	x.lastControl.Store(time.Now().UnixNano())
	x.acceptFrames.Store(true)
	x.workers.Add(2)
	go func() { defer x.workers.Done(); x.videoLoop() }()
	if x.audioEnabled() {
		x.workers.Add(1)
		go func() { defer x.workers.Done(); x.audioLoop() }()
	}
	go func() {
		defer x.workers.Done()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-x.ctx.Done():
				return
			case <-tick.C:
				if time.Since(time.Unix(0, x.lastControl.Load())) > 15*time.Second {
					x.stop("control channel timed out")
					return
				}
			}
		}
	}()
	log.Printf("gamestream: streaming %dx%d@%d %d kbps, audio enabled=%t", x.options.Width, x.options.Height, x.options.FPS, x.options.Bitrate, x.audioEnabled())
	return nil
}

func (s *Server) PushFrame(f bridge.Frame) { s.PushFrameWithStop(f, nil) }

func (s *Server) PushFrameWithStop(f bridge.Frame, stop <-chan struct{}) {
	f.Timing = f.Timing.Enqueue()
	x := s.currentSession()
	if x == nil || x.ctx.Err() != nil {
		return
	}
	x.mu.Lock()
	started := x.started
	x.mu.Unlock()
	if !started {
		return
	}
	if f.Width < 1 || f.Height < 1 || f.Width > 8192 || f.Height > 8192 {
		return
	}
	if f.Format != capture.FormatRaw && f.Format != capture.FormatJPEG && f.Format != capture.FormatH264 && f.Format != capture.FormatHEVC && f.Format != capture.FormatAV1 {
		if !x.acceptFrames.Load() {
			return // Previous protocol's encoder is still being stopped.
		}
		x.stop("capture must provide H.264, HEVC, AV1, Raw or JPEG frames")
		return
	}
	x.framePushMu.Lock()
	defer x.framePushMu.Unlock()
	if x.ctx.Err() != nil {
		return
	}
	select {
	case <-stop:
		return
	default:
	}
	f = f.Retain()
	if f.Format == capture.FormatH264 || f.Format == capture.FormatHEVC || f.Format == capture.FormatAV1 {
		select {
		case x.frames <- f:
			return
		case <-x.ctx.Done():
		case <-stop:
		}
		f.Release()
		return
	}
	select {
	case x.frames <- f:
		return
	default:
	}
	select {
	case old := <-x.frames:
		old.Release()
	default:
	}
	select {
	case x.frames <- f:
		return
	default:
	}
	f.Release()
}
func (s *Server) PushAudio(f capture.AudioFrame) {
	x := s.currentSession()
	if x == nil || x.ctx.Err() != nil || !x.audioEnabled() {
		return
	}
	if f.SampleRate != 48000 || f.Channels != 2 || len(f.PCM) == 0 || len(f.PCM)%4 != 0 {
		return
	}
	f.PCM = append([]byte(nil), f.PCM...)
	select {
	case x.audio <- f:
	default:
	} // bounded, real-time audio never blocks capture
}
func (s *Server) UpdateFramebufferSize(int, int) {} // encoder keeps the negotiated output size
func (s *Server) CloseClients() {
	if x := s.currentSession(); x != nil {
		x.stop("another protocol took control")
		<-x.done
	}
}

func (s *Server) bindStreaming(ip string, base int) error {
	port := func(offset int) int {
		if base == 0 {
			return 0
		}
		return base + offset
	}
	l, err := net.Listen("tcp", net.JoinHostPort(ip, strconv.Itoa(port(21))))
	if err != nil {
		return err
	}
	s.rtspListener = l
	s.rtspPort = l.Addr().(*net.TCPAddr).Port
	addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(ip, strconv.Itoa(port(9))))
	if err != nil {
		s.closeStreamingSockets()
		return err
	}
	s.videoSocket, err = net.ListenUDP("udp", addr)
	if err != nil {
		s.closeStreamingSockets()
		return err
	}
	addr.Port = port(11)
	s.audioSocket, err = net.ListenUDP("udp", addr)
	if err != nil {
		s.closeStreamingSockets()
		return err
	}
	if ip == "" {
		ip = "0.0.0.0"
	}
	s.control, err = native.NewHost(ip, port(10))
	if err != nil {
		s.closeStreamingSockets()
		return err
	}
	s.controlPort = s.control.Port()
	return nil
}
func (s *Server) closeStreamingSockets() {
	if s.rtspListener != nil {
		s.rtspListener.Close()
	}
	if s.videoSocket != nil {
		s.videoSocket.Close()
	}
	if s.audioSocket != nil {
		s.audioSocket.Close()
	}
}
func (s *Server) serveStreaming() {
	s.streamWorkers.Add(4)
	go func() { defer s.streamWorkers.Done(); s.rtspLoop() }()
	go func() { defer s.streamWorkers.Done(); s.pingLoop(s.videoSocket, true) }()
	go func() { defer s.streamWorkers.Done(); s.pingLoop(s.audioSocket, false) }()
	go func() { defer s.streamWorkers.Done(); s.controlLoop() }()
}
func (s *Server) pingLoop(conn *net.UDPConn, video bool) {
	buf := make([]byte, 128)
	for {
		n, peer, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		x := s.currentSession()
		if x == nil || x.ctx.Err() != nil || !sameIP(peer.IP.String(), x.ip) {
			continue
		}
		x.mu.Lock()
		newVideoPeer := false
		valid := n >= 20 && string(buf[:16]) == x.ping
		if x.announced && x.options.Features&2 == 0 && n == 4 && string(buf[:4]) == "PING" {
			valid = true
		}
		if valid {
			if video {
				newVideoPeer = x.videoPeer == nil
				x.videoPeer = peer
			} else {
				x.audioPeer = peer
			}
		}
		x.mu.Unlock()
		if newVideoPeer {
			log.Printf("gamestream: video UDP endpoint ready from %s", peer)
		}
	}
}
func sameIP(a, b string) bool {
	return net.ParseIP(strings.Split(a, "%")[0]).Equal(net.ParseIP(strings.Split(b, "%")[0]))
}

func validateOptions(o streamOptions) error {
	if o.Width <= 0 || o.Height <= 0 || o.Width%2 != 0 || o.Height%2 != 0 || o.FPS < 1 || o.FPS > 120 {
		return errors.New("invalid size/FPS (positive even dimensions, 1..120 FPS)")
	}
	if o.Bitrate < 100 || o.Bitrate > 150000 || o.PacketSize < 512 || o.PacketSize > 1408 || o.MinFEC < 0 || o.MinFEC > 2 {
		return errors.New("invalid bitrate/packet/FEC parameters")
	}
	if o.AudioDuration != 5 && o.AudioDuration != 10 && o.AudioDuration != 20 {
		return fmt.Errorf("unsupported Opus packet duration: %d", o.AudioDuration)
	}
	if o.Encryption & ^7 != 0 {
		return errors.New("unsupported encryption features")
	}
	return nil
}

func (o streamOptions) codecName() string {
	if o.Codec == "av1" {
		return "AV1"
	}
	if o.Codec == "hevc" {
		return "HEVC"
	}
	return "H.264"
}
func (o streamOptions) videoFormat() capture.VideoFormat {
	if o.Codec == "av1" {
		return capture.FormatAV1
	}
	if o.Codec == "hevc" {
		return capture.FormatHEVC
	}
	return capture.FormatH264
}

func (x *streamSession) drainFrames() {
	x.framePushMu.Lock()
	defer x.framePushMu.Unlock()
	for {
		select {
		case f := <-x.frames:
			f.Release()
		default:
			return
		}
	}
}
