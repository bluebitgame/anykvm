package airplay

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"anykvm/internal/airplay/playfair"
	"anykvm/internal/avmedia"
	"anykvm/internal/capture"

	"howett.net/plist"
)

// Screen, rotation, FairPlay, legacy pairing and RAOP on this port. The
// associated AAC-ELD audio remains compressed until the viewer needs PCM.
// HomeKit, buffered audio and HEVC are off.
const features uint64 = 1<<7 | 1<<8 | 1<<9 | 1<<14 | 1<<18 | 1<<19 | 1<<20 | 1<<21 | 1<<27 | 1<<30

// Receiver handles AirPlay discovery, pairing and encrypted H.264 mirroring.
// Each instance supports Start/Close/Start; Close waits for all socket workers.
type Receiver struct {
	name                string
	options             Options
	private             ed25519.PrivateKey
	deviceID, pairingID string
	lifeMu              sync.Mutex
	mu                  sync.Mutex
	listener            net.Listener
	connections         map[net.Conn]struct{}
	advertisement       advertisement
	wg                  sync.WaitGroup
	frames              chan receivedFrame
	audioFrames         chan capture.AudioFrame
	audioOpen           bool
	audioEpoch          uint64
	audioOwner          *pairing // owner with an active audio source
	stop                chan struct{}
	owner               *pairing
	width, height, fps  int
	urlMu               sync.Mutex // Serialize RTP/URL ownership and pairing media lifetime.
	urlPlayer           *urlPlayer
	urlEvents           *reverseEvents
	urlMedia            bool
	browserMedia        bool   // Fixed before Start: URL containers go only to Web.
	fcupNextID          uint64 // Protected by mu; never reused across playback workers.
}

type receivedFrame struct {
	frame *capture.Frame
	owner *pairing
}

func NewReceiver(device string, options Options) (*Receiver, error) {
	return newReceiver(device, options, avmedia.URLMediaAvailable() == nil)
}

func newReceiver(device string, options Options, urlMedia bool) (*Receiver, error) {
	name, err := ReceiverName(device)
	if err != nil {
		return nil, err
	}
	if err := options.Validate(); err != nil {
		return nil, err
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	// Stable, locally administered device ID; signing key lives for this object.
	hostname, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256([]byte(hostname + "\x00" + name + "\x00" + options.address()))
	id := append(net.HardwareAddr(nil), hash[:6]...)
	id[0] = (id[0] | 2) & 0xfe
	return &Receiver{name: name, options: options, private: private, deviceID: id.String(), pairingID: fmt.Sprintf("%x-%x-%x-%x-%x", hash[:4], hash[4:6], hash[6:8], hash[8:10], hash[10:16]), width: 1920, height: 1080, fps: 30, urlMedia: urlMedia}, nil
}

func (r *Receiver) textRecords() []string {
	return []string{"deviceid=" + r.deviceID, "features=" + r.featureRecord(), "flags=0x4", "model=AppleTV3,2", "srcvers=220.68", "vv=2", "pk=" + hex.EncodeToString(r.private.Public().(ed25519.PublicKey)), "pi=" + r.pairingID, "pw=false"}
}

// Start returns once discovery and the control listener are ready. It never
// waits for an iPhone or a viewer.
func (r *Receiver) Start() error {
	r.lifeMu.Lock()
	defer r.lifeMu.Unlock()
	r.mu.Lock()
	active := r.listener != nil
	r.mu.Unlock()
	if active {
		return nil
	}
	if err := playfair.Available(); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", r.options.address())
	if err != nil {
		return fmt.Errorf("airplay: control listener: %w", err)
	}
	ifaces, err := r.advertisementInterfaces()
	if err != nil {
		listener.Close()
		return err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	advertisement, err := r.registerAdvertisements(port, ifaces)
	if err != nil {
		listener.Close()
		return fmt.Errorf("airplay: mDNS advertisement: %w", err)
	}
	r.start(listener, advertisement)
	return nil
}

func (r *Receiver) advertisementInterfaces() ([]net.Interface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	host, _, _ := net.SplitHostPort(r.options.address())
	bindIP := net.ParseIP(host)
	var selected []net.Interface
	for _, iface := range ifaces {
		if r.options.Interface != "" && iface.Name != r.options.Interface {
			continue
		}
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return nil, err
		}
		// Automatic LAN discovery must not publish unusable VPN, inactive or
		// peer-to-peer interface addresses to a phone on the Wi-Fi network.
		if r.options.Interface == "" {
			lan := false
			for _, address := range addrs {
				ip, _, _ := net.ParseCIDR(address.String())
				lan = lan || ip.IsGlobalUnicast() && !ip.IsLoopback()
			}
			if !lan || iface.Flags&net.FlagPointToPoint != 0 {
				continue
			}
		}
		if bindIP != nil && !bindIP.IsUnspecified() {
			matches := false
			for _, a := range addrs {
				ip, _, _ := net.ParseCIDR(a.String())
				matches = matches || ip.Equal(bindIP)
			}
			if !matches {
				continue
			}
		}
		selected = append(selected, iface)
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("airplay: no active multicast interface matches airplay.addr/interface")
	}
	return selected, nil
}

func (r *Receiver) start(listener net.Listener, advertisement advertisement) {
	r.mu.Lock()
	r.listener, r.advertisement = listener, advertisement
	r.connections = make(map[net.Conn]struct{})
	r.frames, r.stop = make(chan receivedFrame, 2), make(chan struct{})
	r.audioFrames, r.audioOpen = make(chan capture.AudioFrame, 16), false
	r.audioOwner = nil
	r.owner = nil
	r.wg.Add(1)
	r.mu.Unlock()
	go r.accept(listener)
}

func (r *Receiver) accept(listener net.Listener) {
	defer r.wg.Done()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		r.mu.Lock()
		if r.listener != listener || len(r.connections) >= 8 {
			full := r.listener == listener && len(r.connections) >= 8
			r.mu.Unlock()
			if full {
				log.Printf("airplay: control connection from %s rejected: connection limit reached", conn.RemoteAddr())
			}
			conn.Close()
			continue
		}
		r.connections[conn] = struct{}{}
		r.wg.Add(1)
		r.mu.Unlock()
		go r.serve(conn)
	}
}

func (r *Receiver) serve(conn net.Conn) {
	defer r.wg.Done()
	defer func() { conn.Close(); r.mu.Lock(); delete(r.connections, conn); r.mu.Unlock() }()
	p := pairing{private: r.private, control: conn}
	defer func() {
		r.urlMu.Lock()
		defer r.urlMu.Unlock()
		if p.media != nil {
			p.media.Close()
		}
		r.mu.Lock()
		if r.owner == &p {
			r.owner = nil
		}
		r.mu.Unlock()
	}()
	reader := bufio.NewReaderSize(conn, maxControlHeaders)
	for {
		q, err := r.readControlRequest(conn, reader, 30*time.Second)
		if err != nil {
			log.Printf("airplay: control connection from %s ended: %v", conn.RemoteAddr(), err)
			return
		}
		if q.method == "POST" && q.path == "/reverse" {
			r.serveReverse(q, conn, reader)
			return
		}
		status, body, contentType := r.handle(q, &p, conn)
		if status != 200 || q.path != "/feedback" && q.path != "/action" && q.method != "GET_PARAMETER" && q.method != "SET_PARAMETER" {
			log.Printf("airplay: %s %q from %s -> %d%s", q.method, q.path, conn.RemoteAddr(), status, controlDetails(q))
		}
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := writeResponse(conn, q, status, body, contentType); err != nil {
			log.Printf("airplay: %s %q response to %s failed: %v", q.method, q.path, conn.RemoteAddr(), err)
			return
		}
		if q.method == "TEARDOWN" && status == 200 && p.media == nil || status == 403 || q.headers.Get("Connection") == "close" {
			return
		}
	}
}

// Log only the fields needed to distinguish playback-mode changes from session
// shutdown. Never dump control bodies: pairing and SETUP contain key material.
func controlDetails(q request) string {
	if q.method != "TEARDOWN" && !(q.method == "POST" && q.path == "/audioMode") {
		return ""
	}
	if len(q.body) == 0 {
		return " body=empty"
	}
	var details struct {
		AudioMode string `plist:"audioMode"`
		Streams   []struct {
			Type uint64 `plist:"type"`
		} `plist:"streams"`
	}
	if _, err := plist.Unmarshal(q.body, &details); err != nil {
		return fmt.Sprintf(" body=invalid-plist bytes=%d", len(q.body))
	}
	if q.method == "TEARDOWN" {
		if details.Streams == nil {
			return " streams=absent"
		}
		types := make([]uint64, 0, min(len(details.Streams), 8))
		for _, stream := range details.Streams[:min(len(details.Streams), 8)] {
			types = append(types, stream.Type)
		}
		return fmt.Sprintf(" streams=%v count=%d (96=audio, 110=mirror)", types, len(details.Streams))
	}
	return fmt.Sprintf(" audio_mode=%.128q", details.AudioMode)
}

// Mirror senders keep control and event connections idle while video flows.
// Preserve idle connections from the active sender, but bound the time allowed
// to complete a request once its first byte arrives.
func (r *Receiver) readControlRequest(conn net.Conn, reader *bufio.Reader, timeout time.Duration) (request, error) {
	for {
		conn.SetReadDeadline(time.Now().Add(timeout))
		if _, err := reader.Peek(1); err != nil {
			if e, ok := err.(net.Error); ok && e.Timeout() && r.activeSenderConnection(conn) {
				continue
			}
			return request{}, err
		}
		conn.SetReadDeadline(time.Now().Add(timeout))
		return readRequest(reader)
	}
}

func (r *Receiver) activeSenderConnection(conn net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.owner == nil || r.owner.control == nil {
		return false
	}
	peer, ok := conn.RemoteAddr().(*net.TCPAddr)
	owner, ownerOK := r.owner.control.RemoteAddr().(*net.TCPAddr)
	return ok && ownerOK && peer.IP.Equal(owner.IP)
}

func (r *Receiver) handle(q request, p *pairing, conn net.Conn) (int, []byte, string) {
	if status, body, contentType, handled := r.handleURL(q, conn); handled {
		return status, body, contentType
	}
	if q.method == "OPTIONS" {
		return 200, nil, ""
	}
	if q.method == "TEARDOWN" {
		return r.teardown(q, p), nil, ""
	}
	if q.method == "GET" && q.path == "/info" {
		if len(q.body) != 0 {
			var query struct {
				Qualifier []string `plist:"qualifier"`
			}
			if _, err := plist.Unmarshal(q.body, &query); err != nil {
				return 400, nil, ""
			}
			result := map[string]any{}
			for _, field := range query.Qualifier {
				switch field {
				case "txtAirPlay":
					result[field] = encodeTXT(r.textRecords())
				case "txtRAOP":
					result[field] = encodeTXT(r.raopRecords())
				}
			}
			log.Printf("airplay: info qualifiers=%q features=%s", query.Qualifier, r.featureRecord())
			body, err := plist.Marshal(result, plist.BinaryFormat)
			if err != nil {
				return 400, nil, ""
			}
			return 200, body, "application/x-apple-binary-plist"
		}
		r.mu.Lock()
		width, height, fps := r.width, r.height, r.fps
		r.mu.Unlock()
		log.Printf("airplay: info capabilities features=%s video=%t hls=%t web_media=%t", r.featureRecord(), r.featureBits()&(1<<0) != 0, r.featureBits()&(1<<4) != 0, r.browserMedia)
		body, err := plist.Marshal(map[string]any{
			"name": r.name, "deviceID": r.deviceID, "macAddress": r.deviceID,
			"model": "AppleTV3,2", "sourceVersion": "220.68", "features": r.featureBits(),
			"statusFlags": uint64(4), "vv": uint64(2), "pi": r.pairingID,
			"pk":            []byte(r.private.Public().(ed25519.PublicKey)),
			"txtAirPlay":    encodeTXT(r.textRecords()),
			"txtRAOP":       encodeTXT(r.raopRecords()),
			"initialVolume": 0.0,
			"audioFormats": []any{
				map[string]any{"type": uint64(100), "audioInputFormats": uint64(0), "audioOutputFormats": supportedAudioFormats},
				map[string]any{"type": uint64(101), "audioInputFormats": uint64(0), "audioOutputFormats": supportedAudioFormats},
			},
			"audioLatencies": []any{
				map[string]any{"type": uint64(100), "audioType": "default", "inputLatencyMicros": uint64(0), "outputLatencyMicros": uint64(0)},
				map[string]any{"type": uint64(101), "audioType": "default", "inputLatencyMicros": uint64(0), "outputLatencyMicros": uint64(0)},
			},
			"displays": []any{map[string]any{
				"uuid": r.pairingID, "width": uint64(width), "height": uint64(height),
				"widthPixels": uint64(width), "heightPixels": uint64(height),
				"widthPhysical": uint64(0), "heightPhysical": uint64(0),
				"refreshRate": 1.0 / float64(fps), "maxFPS": uint64(fps), "rotation": false,
				"overscanned": false, "features": uint64(14),
			}},
			"keepAliveLowPower": true, "keepAliveSendStatsAsBody": true,
		}, plist.BinaryFormat)
		if err != nil {
			return 400, nil, ""
		}
		return 200, body, "application/x-apple-binary-plist"
	}
	if q.method == "POST" && (q.path == "/pair-setup" || q.path == "/pair-verify") {
		var body []byte
		var err error
		if q.path == "/pair-setup" {
			body, err = p.pairSetup(q.body)
		} else {
			body, err = p.pairVerify(q.body)
		}
		if err != nil {
			log.Printf("airplay: pairing %q from %s failed: %v", q.path, conn.RemoteAddr(), err)
			return 403, nil, ""
		}
		return 200, body, "application/octet-stream"
	}
	if q.method == "POST" && q.path == "/fp-setup" {
		body, err := p.fpSetup(q.body)
		if err != nil {
			return 403, nil, ""
		}
		return 200, body, "application/octet-stream"
	}
	if q.method == "SETUP" {
		return r.setup(q, p, conn)
	}
	if q.method == "RECORD" || q.method == "FLUSH" || q.method == "GET_PARAMETER" || q.method == "SET_PARAMETER" || q.method == "POST" && (q.path == "/feedback" || q.path == "/audioMode") {
		if !p.verified {
			return 403, nil, ""
		}
		if q.method == "GET_PARAMETER" && strings.TrimSpace(string(q.body)) == "volume" {
			db := 0.0
			if p.media != nil {
				p.media.mu.Lock()
				db = p.media.audioVolumeDB
				p.media.mu.Unlock()
			}
			return 200, []byte(fmt.Sprintf("volume: %.6f\r\n", db)), "text/parameters"
		}
		if p.media != nil {
			if q.method == "FLUSH" {
				var next *uint16
				for _, field := range strings.Split(q.headers.Get("RTP-Info"), ";") {
					if value, ok := strings.CutPrefix(strings.TrimSpace(field), "seq="); ok {
						if sequence, err := strconv.ParseUint(value, 10, 16); err == nil {
							seq := uint16(sequence)
							next = &seq
						}
					}
				}
				p.media.flushAudio(next)
			}
			if q.method == "SET_PARAMETER" {
				if value, ok := strings.CutPrefix(strings.TrimSpace(string(q.body)), "volume:"); ok {
					if db, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
						p.media.setAudioVolume(db)
					}
				}
			}
		}
		return 200, nil, ""
	}
	return 404, nil, ""
}

// A stream-specific teardown is also used during playback-mode changes. Keep
// pairing, timing and the other stream alive so the sender can SETUP again.
func (r *Receiver) teardown(q request, p *pairing) int {
	r.urlMu.Lock()
	defer r.urlMu.Unlock()
	if !p.verified {
		return 403
	}
	var request struct {
		Streams []struct {
			Type uint64 `plist:"type"`
		} `plist:"streams"`
	}
	if len(q.body) != 0 {
		if _, err := plist.Unmarshal(q.body, &request); err != nil {
			return 400
		}
	}
	seen := map[uint64]bool{}
	for _, stream := range request.Streams {
		if seen[stream.Type] || stream.Type != 96 && stream.Type != 110 {
			return 461
		}
		seen[stream.Type] = true
	}
	if len(request.Streams) == 0 {
		if p.media != nil {
			p.media.Close()
			p.media = nil
		}
		r.mu.Lock()
		if r.owner == p {
			r.owner = nil
		}
		r.mu.Unlock()
	} else if p.media != nil {
		if seen[96] {
			p.media.stopAudio()
		}
		if seen[110] {
			p.media.stopMirror()
		}
		// Stream-specific teardown keeps pairing and timing reusable, but an
		// empty RTP session must allow the sender to start HTTP video playback.
		p.media.mu.Lock()
		idle := p.media.listener == nil && p.media.audioData == nil
		p.media.mu.Unlock()
		if idle {
			r.mu.Lock()
			if r.owner == p {
				r.owner = nil
			}
			r.mu.Unlock()
		}
	}
	return 200
}

// End media ownership without unregistering discovery or closing the listener.
func (r *Receiver) disconnectSenders() {
	r.stopURL()
	r.mu.Lock()
	r.owner = nil
	r.audioOwner = nil
	r.clearAudioLocked()
	connections := make([]net.Conn, 0, len(r.connections))
	for conn := range r.connections {
		connections = append(connections, conn)
	}
	r.mu.Unlock()
	for _, conn := range connections {
		conn.Close()
	}
}

func (r *Receiver) Close() error {
	r.lifeMu.Lock()
	defer r.lifeMu.Unlock()
	r.mu.Lock()
	listener, advertisement := r.listener, r.advertisement
	r.listener, r.advertisement = nil, nil
	if r.stop != nil {
		select {
		case <-r.stop:
		default:
			close(r.stop)
		}
	}
	connections := make([]net.Conn, 0, len(r.connections))
	for conn := range r.connections {
		connections = append(connections, conn)
	}
	r.mu.Unlock()
	r.stopURL()
	if listener != nil {
		listener.Close()
	}
	for _, conn := range connections {
		conn.Close()
	}
	if advertisement != nil {
		advertisement.Shutdown()
	}
	r.wg.Wait()
	r.mu.Lock()
	r.frames = nil
	r.audioFrames, r.audioOpen = nil, false
	r.audioOwner = nil
	r.owner = nil
	r.mu.Unlock()
	return nil
}

func (r *Receiver) ReadFrame() (*capture.Frame, error) {
	r.mu.Lock()
	frames, stop := r.frames, r.stop
	r.mu.Unlock()
	if stop == nil {
		return nil, capture.ErrStreamStopped
	}
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return nil, capture.ErrStreamStopped
		case <-timer.C:
			return nil, nil
		case received := <-frames:
			r.mu.Lock()
			active := r.owner == received.owner
			r.mu.Unlock()
			if active {
				return received.frame, nil
			}
			received.frame.Release()
		}
	}
}

func (r *Receiver) setup(q request, p *pairing, conn net.Conn) (int, []byte, string) {
	r.urlMu.Lock()
	defer r.urlMu.Unlock()
	if !p.verified || len(p.fairplayMessage) != 164 {
		return 403, nil, ""
	}
	var request setupRequest
	if _, err := plist.Unmarshal(q.body, &request); err != nil {
		log.Printf("airplay: invalid SETUP property list: %v", err)
		return 400, nil, ""
	}
	var streamTypes []uint64
	for _, stream := range request.Streams {
		streamTypes = append(streamTypes, stream.Type)
	}
	log.Printf("airplay: SETUP streams=%v timing=%q ekey_bytes=%d eiv_bytes=%d", streamTypes, request.TimingProtocol, len(request.EKey), len(request.EIV))
	if len(request.Streams) > 2 || request.TimingProtocol != "" && request.TimingProtocol != "NTP" {
		return 461, nil, ""
	}
	seen := map[uint64]bool{}
	for _, stream := range request.Streams {
		if seen[stream.Type] || stream.Type != 110 && stream.Type != 96 {
			return 461, nil, ""
		}
		seen[stream.Type] = true
		if stream.Type == 96 {
			log.Printf("airplay: audio SETUP ct=%d spf=%d format=0x%x sr=%d control=%d using_screen=%t is_media=%t", stream.CT, stream.SPF, stream.AudioFormat, stream.SampleRate, stream.ControlPort, stream.UsingScreen, stream.IsMedia)
			if _, err := stream.audioOptions().normalized(); err != nil {
				log.Printf("airplay: audio SETUP rejected: %v", err)
				return 461, nil, ""
			}
		}
	}
	if p.media == nil {
		if len(request.EKey) != 72 || len(request.EIV) != 16 || request.TimingPort < 1 || request.TimingPort > 65535 || conn == nil {
			return 400, nil, ""
		}
		key, err := playfair.Decrypt(p.fairplayMessage, request.EKey)
		if err != nil {
			return 400, nil, ""
		}
		r.mu.Lock()
		if r.owner != nil && r.owner != p {
			r.mu.Unlock()
			return 453, nil, ""
		}
		r.owner = p
		r.mu.Unlock()
		media, err := newMediaSession(r, p, conn, key, uint16(request.TimingPort))
		if err != nil {
			r.mu.Lock()
			if r.owner == p {
				r.owner = nil
			}
			r.mu.Unlock()
			log.Printf("airplay: media setup failed: %v", err)
			return 400, nil, ""
		}
		p.media = media
		p.media.audioIV = append([]byte(nil), request.EIV...)
	} else if len(request.EKey) != 0 || len(request.EIV) != 0 {
		return 400, nil, ""
	} else {
		// A retained pairing may restart after releasing all its streams.
		// Reclaim ownership before opening sockets; an intervening URL or RTP
		// session must not be displaced by this old control connection.
		r.mu.Lock()
		if r.owner != nil && r.owner != p {
			r.mu.Unlock()
			return 453, nil, ""
		}
		r.owner = p
		r.mu.Unlock()
	}
	// This receiver does not implement the separate RTP event channel. It is
	// unrelated to HTTP /reverse; advertising the control listener here falsely
	// asks the sender to establish an event connection that we cannot serve.
	response := map[string]any{"timingPort": uint64(p.media.timing.port()), "eventPort": uint64(0)}
	log.Printf("airplay: SETUP transport timing_port=%d event_port=0", p.media.timing.port())
	if len(request.Streams) > 0 {
		var streams []any
		for _, stream := range request.Streams {
			if stream.Type == 110 {
				port, err := p.media.startMirror(stream.ConnectionID)
				if err != nil {
					return 400, nil, ""
				}
				streams = append(streams, map[string]any{"type": uint64(110), "dataPort": uint64(port)})
			} else {
				dataPort, controlPort, err := p.media.startAudio(stream.audioOptions())
				if err != nil {
					log.Printf("airplay: audio start failed: %v", err)
					return 400, nil, ""
				}
				streams = append(streams, map[string]any{"type": uint64(96), "dataPort": uint64(dataPort), "controlPort": uint64(controlPort)})
			}
		}
		response["streams"] = streams
	}
	body, err := plist.Marshal(response, plist.BinaryFormat)
	if err != nil {
		return 400, nil, ""
	}
	return 200, body, "application/x-apple-binary-plist"
}

type setupRequest struct {
	EKey           []byte        `plist:"ekey"`
	EIV            []byte        `plist:"eiv"`
	TimingPort     uint64        `plist:"timingPort"`
	TimingProtocol string        `plist:"timingProtocol"`
	Streams        []setupStream `plist:"streams"`
}

type setupStream struct {
	Type         uint64 `plist:"type"`
	ConnectionID uint64 `plist:"streamConnectionID"`
	CT           uint64 `plist:"ct"`
	SPF          uint64 `plist:"spf"`
	AudioFormat  uint64 `plist:"audioFormat"`
	ControlPort  uint64 `plist:"controlPort"`
	SampleRate   uint64 `plist:"sr"`
	UsingScreen  bool   `plist:"usingScreen"`
	IsMedia      bool   `plist:"isMedia"`
}

func (s setupStream) audioOptions() audioOptions {
	return audioOptions{CT: s.CT, SPF: s.SPF, Format: s.AudioFormat, ControlPort: s.ControlPort, SampleRate: s.SampleRate}
}
