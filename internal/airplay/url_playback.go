package airplay

import (
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"anykvm/internal/avmedia"
	"anykvm/internal/capture"
	"anykvm/internal/logging"
	"howett.net/plist"
)

type playbackKey struct{ peer, session string }

func mediaKey(q request, conn net.Conn) (playbackKey, error) {
	if conn == nil {
		return playbackKey{}, fmt.Errorf("missing media connection")
	}
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return playbackKey{}, err
	}
	id := q.headers.Get("X-Apple-Session-ID")
	if len(id) > 128 || strings.ContainsAny(id, "\r\n\x00") {
		return playbackKey{}, fmt.Errorf("invalid session ID")
	}
	return playbackKey{host, id}, nil
}

// HTTP video uses a separate connection and session ID from the paired RTP
// control. Compare its network peer without treating a new port as a new sender.
func samePlaybackPeer(owner *pairing, key playbackKey) bool {
	if owner == nil || owner.control == nil {
		return false
	}
	host, _, err := net.SplitHostPort(owner.control.RemoteAddr().String())
	return err == nil && host == key.peer
}

type playRequest struct {
	Location string   `plist:"Content-Location"`
	Start    float64  `plist:"Start-Position"`
	Seconds  *float64 `plist:"Start-Position-Seconds,omitempty"`
}

func parsePlay(q request) (playRequest, error) {
	var p playRequest
	if strings.Contains(q.headers.Get("Content-Type"), "plist") || strings.HasPrefix(string(q.body), "bplist00") {
		if _, err := plist.Unmarshal(q.body, &p); err != nil {
			return p, err
		}
	} else {
		seen := map[string]bool{}
		for _, line := range strings.Split(string(q.body), "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
			if !ok {
				continue
			}
			v = strings.TrimSpace(v)
			if seen[k] {
				return p, fmt.Errorf("duplicate media parameter")
			}
			seen[k] = true
			switch k {
			case "Content-Location":
				p.Location = v
			case "Start-Position":
				var err error
				p.Start, err = strconv.ParseFloat(v, 64)
				if err != nil {
					return p, err
				}
			case "Start-Position-Seconds":
				seconds, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return p, err
				}
				p.Seconds = &seconds
			}
		}
	}
	u, err := url.Parse(p.Location)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || len(p.Location) > 16384 || strings.ContainsAny(p.Location, "\r\n\x00") || (u.Scheme != "http" && u.Scheme != "https" && !validFCUPURL(u)) || math.IsNaN(p.Start) || math.IsInf(p.Start, 0) || p.Start < 0 || p.Start > 1 || p.Seconds != nil && (math.IsNaN(*p.Seconds) || math.IsInf(*p.Seconds, 0) || *p.Seconds < 0 || *p.Seconds >= float64(math.MaxInt64)/1e6) {
		return p, fmt.Errorf("invalid media location/start")
	}
	return p, nil
}

type playbackCommand struct {
	seek  bool
	value float64
	reply chan error
}
type urlPlayer struct {
	receiver           *Receiver
	key                playbackKey
	owner              *pairing
	ctx                context.Context
	cancel             context.CancelFunc
	done               chan struct{}
	commands           chan playbackCommand
	mu                 sync.Mutex
	state              string
	duration, position float64
	ready              bool
	fcup               *fcupStore
	browser            *browserPlayback
}

func (p *urlPlayer) setState(state string) {
	p.mu.Lock()
	changed := p.state != state
	p.state = state
	p.mu.Unlock()
	if changed {
		p.receiver.sendMediaEvent(p.key, state)
		logging.Infof("airplay: URL playback %s", state)
	}
}
func (p *urlPlayer) info() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	rate := 0.0
	if p.state == "playing" {
		rate = 1
	}
	ranges := []any{}
	// Duration is a timeline bound, not a claim that the entire URL is buffered.
	if p.duration > 0 {
		ranges = append(ranges, map[string]any{"start": 0.0, "duration": p.duration})
	}
	loaded := []any{}
	if p.browser != nil {
		loaded = playbackRanges(p.browser.buffered)
		ranges = playbackRanges(p.browser.seekable)
	}
	return map[string]any{"duration": p.duration, "position": p.position, "rate": rate, "readyToPlay": p.ready, "playbackBufferEmpty": !p.ready || p.state == "loading", "playbackBufferFull": false, "playbackLikelyToKeepUp": p.ready && p.state != "loading", "loadedTimeRanges": loaded, "seekableTimeRanges": ranges}
}
func (p *urlPlayer) close() { p.cancel(); <-p.done }
func (p *urlPlayer) command(seek bool, value float64) error {
	if p.browser != nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.ctx.Err() != nil {
			return context.Canceled
		}
		p.browser.version++
		if seek {
			p.browser.seekVersion = p.browser.version
			p.browser.position, p.browser.fraction = value, 0
			p.position = value
		} else {
			p.browser.paused = value == 0
			if p.ready {
				p.browser.position, p.browser.fraction = p.position, 0
			}
		}
		return nil
	}
	c := playbackCommand{seek: seek, value: value, reply: make(chan error, 1)}
	timer := time.NewTimer(12 * time.Second)
	defer timer.Stop()
	select {
	case p.commands <- c:
	case <-p.ctx.Done():
		return context.Canceled
	case <-p.done:
		return fmt.Errorf("playback worker stopped")
	case <-timer.C:
		return context.DeadlineExceeded
	}
	select {
	case err := <-c.reply:
		return err
	case <-p.ctx.Done():
		return context.Canceled
	case <-p.done:
		return fmt.Errorf("playback worker stopped")
	case <-timer.C:
		return context.DeadlineExceeded
	}
}

// Only the playback worker accesses the demuxer. Commands also wake bounded
// video backpressure, paused playback and EOF, so controls work without a viewer.
func (p *urlPlayer) run(request playRequest) {
	defer close(p.done)
	defer func() { p.setState("stopped") }()
	if p.fcup != nil {
		defer p.fcup.close()
		location, err := p.fcup.open(request.Location)
		if err != nil {
			logging.Infof("airplay: FCUP resource service unavailable")
			return
		}
		request.Location = location
	}
	source, err := avmedia.OpenMedia(p.ctx, request.Location)
	if err != nil {
		if p.ctx.Err() == nil {
			logging.Infof("airplay: URL media open failed: %v", err)
		}
		return
	}
	defer source.Close()
	p.mu.Lock()
	p.duration = source.Duration()
	p.ready = true
	p.mu.Unlock()
	origin := request.Start * source.Duration()
	if request.Seconds != nil {
		origin = *request.Seconds
	}
	if origin > 0 {
		if source.Seek(origin) != nil {
			logging.Infof("airplay: URL initial seek failed")
			return
		}
	}
	p.mu.Lock()
	p.position = origin
	p.mu.Unlock()
	anchor := time.Now()
	pauseStarted := time.Time{}
	paused := false
	ended := false
	p.setState("playing")
	var packet *avmedia.MediaPacket
	defer func() {
		if packet != nil {
			packet.Release()
		}
	}()
	apply := func(c playbackCommand) {
		var err error
		if c.seek {
			err = source.Seek(c.value)
			if err == nil {
				if packet != nil {
					packet.Release()
					packet = nil
				}
				origin = c.value
				anchor = time.Now()
				if paused {
					pauseStarted = anchor
				}
				ended = false
				p.receiver.clearURLFrames(p.owner)
				p.mu.Lock()
				p.position = c.value
				p.mu.Unlock()
				if paused {
					p.setState("paused")
				} else {
					p.setState("playing")
				}
			}
		} else {
			next := c.value == 0
			if paused && !next {
				anchor = anchor.Add(time.Since(pauseStarted))
			}
			if !paused && next {
				pauseStarted = time.Now()
			}
			paused = next
			if paused {
				p.setState("paused")
			} else if !ended {
				p.setState("playing")
			}
		}
		c.reply <- err
	}
	for {
		select {
		case <-p.ctx.Done():
			return
		case c := <-p.commands:
			apply(c)
			continue
		default:
		}
		if paused || ended {
			select {
			case <-p.ctx.Done():
				return
			case c := <-p.commands:
				apply(c)
			}
			continue
		}
		if packet == nil {
			packet, err = source.Read()
			if err == io.EOF {
				ended = true
				p.setState("stopped")
				continue
			}
			if err != nil {
				logging.Infof("airplay: URL media read failed: %v", err)
				return
			}
			if packet == nil {
				continue
			}
		}
		due := anchor.Add(time.Duration((float64(packet.DecodeTimestamp)/1e6 - origin) * float64(time.Second)))
		if delay := time.Until(due); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-p.ctx.Done():
				timer.Stop()
				return
			case c := <-p.commands:
				timer.Stop()
				apply(c)
				continue
			case <-timer.C:
			}
		}
		if packet.Audio {
			if len(packet.Config) < 2 || packet.Config[0]>>3 != 2 || packet.SampleRate < 8000 || packet.SampleRate > 192000 || packet.Channels < 1 || packet.Channels > 8 {
				logging.Infof("airplay: URL audio requires AAC-LC with AudioSpecificConfig")
				return
			}
			// Copy into the bounded Go audio queue before releasing native ownership.
			p.receiver.mu.Lock()
			if p.receiver.owner == p.owner {
				p.receiver.audioOwner = p.owner
			}
			p.receiver.mu.Unlock()
			p.receiver.pushAudio(p.owner, capture.AudioFrame{Encoded: append([]byte(nil), packet.Data...), Codec: packet.Codec, Config: packet.Config, SampleRate: packet.SampleRate, Channels: packet.Channels, FramesPerPacket: 1024, Timestamp: packet.Timestamp})
		} else {
			frame := &capture.Frame{Data: packet.Data, Buffer: packet.Buffer, Format: capture.FormatH264, Width: packet.Width, Height: packet.Height, CapturedAt: anchor.Add(time.Duration((float64(packet.Timestamp)/1e6 - origin) * float64(time.Second)))}
			p.receiver.mu.Lock()
			frames, stop := p.receiver.frames, p.receiver.stop
			p.receiver.mu.Unlock()
			select {
			case frames <- receivedFrame{frame: frame, owner: p.owner}:
				packet.Buffer = nil
			case <-p.ctx.Done():
				return
			case <-stop:
				return
			case c := <-p.commands:
				apply(c)
				continue
			}
		}
		p.mu.Lock()
		p.position = max(p.position, float64(packet.Timestamp)/1e6)
		if p.duration > 0 {
			p.position = min(p.position, p.duration)
		}
		p.mu.Unlock()
		packet.Release()
		packet = nil
	}
}

func (r *Receiver) clearURLFrames(owner *pairing) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.owner != owner {
		return
	}
	r.clearAudioLocked()
	for {
		select {
		case old := <-r.frames:
			old.frame.Release()
		default:
			return
		}
	}
}
func (r *Receiver) stopURLLocked() {
	r.mu.Lock()
	p := r.urlPlayer
	r.mu.Unlock()
	if p == nil {
		return
	}
	p.close()
	r.clearURLFrames(p.owner)
	r.mu.Lock()
	if r.urlPlayer == p {
		r.urlPlayer = nil
	}
	if r.owner == p.owner {
		r.owner = nil
		r.audioOwner = nil
	}
	r.mu.Unlock()
}
func (r *Receiver) stopURL() {
	// Cancel before taking the control lock: a request may be waiting on a
	// blocked demuxer while shutdown needs that same lock.
	r.mu.Lock()
	p := r.urlPlayer
	r.mu.Unlock()
	if p != nil {
		p.cancel()
	}
	r.urlMu.Lock()
	defer r.urlMu.Unlock()
	r.stopURLLocked()
}

func (r *Receiver) handleURL(q request, conn net.Conn) (int, []byte, string, bool) {
	if q.method == "POST" && q.path == "/action" {
		return r.handleFCUPAction(q, conn)
	}
	if q.method == "GET" && q.path == "/server-info" {
		body, _ := plist.Marshal(map[string]any{"deviceid": r.deviceID, "features": r.featureBits(), "model": "AppleTV3,2", "protovers": "1.0", "srcvers": "220.68"}, plist.XMLFormat)
		return 200, body, "text/x-apple-plist+xml", true
	}
	matched := q.method == "POST" && (q.path == "/play" || q.path == "/stop" || q.path == "/rate" || q.path == "/scrub") || q.method == "GET" && (q.path == "/scrub" || q.path == "/playback-info")
	if !matched {
		return 0, nil, "", false
	}
	key, err := mediaKey(q, conn)
	if err != nil {
		return 400, nil, "", true
	}
	r.urlMu.Lock()
	defer r.urlMu.Unlock()
	r.mu.Lock()
	player, owner, listener := r.urlPlayer, r.owner, r.listener
	r.mu.Unlock()
	// A paired sender commonly establishes RTP timing before the separate
	// HTTP /reverse and /play sockets. Allow its video request to switch modes,
	// while controls for an existing URL player still require its exact key.
	transition := q.path == "/play" && player == nil && owner != nil && owner.verified && owner.media != nil && key.session != "" && samePlaybackPeer(owner, key)
	if owner != nil && (player == nil || player.key != key) && !transition {
		return 453, nil, "", true
	}
	if q.path == "/play" {
		request, err := parsePlay(q)
		if err != nil {
			return 400, nil, "", true
		}
		if (!r.urlMedia && !r.browserMedia) || listener == nil {
			return 503, nil, "", true
		}
		if u, _ := url.Parse(request.Location); validFCUPURL(u) && key.session == "" {
			return 400, nil, "", true
		}
		r.stopURLLocked()
		ctx, cancel := context.WithCancel(context.Background())
		p := &urlPlayer{receiver: r, key: key, owner: &pairing{control: conn}, ctx: ctx, cancel: cancel, done: make(chan struct{}), commands: make(chan playbackCommand), state: "loading"}
		if u, _ := url.Parse(request.Location); validFCUPURL(u) {
			p.fcup = newFCUPStore(ctx, r, key, u)
		}
		if r.browserMedia {
			if err := p.initBrowser(request); err != nil {
				cancel()
				return 503, nil, "", true
			}
		}
		if transition {
			// Keep pairing and timing reusable. urlMu serializes SETUP, teardown
			// and control cleanup so sockets cannot restart during this switch.
			owner.media.stopAudio()
			owner.media.stopMirror()
			r.mu.Lock()
			if r.owner == owner {
				r.owner = nil
			}
			r.mu.Unlock()
			logging.Infof("airplay: paired RTP sender switched to HTTP video")
		}
		r.mu.Lock()
		// Shutdown or viewer detach may cancel ownership while URL setup runs.
		if r.listener != listener {
			r.mu.Unlock()
			cancel()
			return 503, nil, "", true
		}
		if r.owner != nil {
			r.mu.Unlock()
			cancel()
			return 453, nil, "", true
		}
		r.urlPlayer = p
		r.owner = p.owner
		r.audioOwner = nil
		r.clearAudioLocked()
		r.mu.Unlock()
		r.sendMediaEvent(key, "loading")
		if p.fcup != nil {
			logging.Infof("airplay: FCUP playback requested")
		}
		if p.browser != nil {
			go p.runBrowser()
		} else {
			go p.run(request)
		}
		return 200, nil, "", true
	}
	if q.path == "/stop" {
		r.stopURLLocked()
		return 200, nil, "", true
	}
	info := map[string]any{"duration": 0.0, "position": 0.0, "rate": 0.0, "readyToPlay": false}
	if player != nil {
		info = player.info()
	}
	if q.method == "GET" {
		if q.path == "/scrub" {
			return 200, []byte(fmt.Sprintf("duration: %.6f\r\nposition: %.6f\r\n", info["duration"], info["position"])), "text/parameters", true
		}
		body, _ := plist.Marshal(info, plist.XMLFormat)
		return 200, body, "text/x-apple-plist+xml", true
	}
	name := "value"
	seek := q.path == "/scrub"
	if seek {
		name = "position"
	}
	values := q.query[name]
	if len(values) != 1 {
		return 400, nil, "", true
	}
	value, err := strconv.ParseFloat(values[0], 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > float64(math.MaxInt64)/1e6 || !seek && value != 0 && value != 1 {
		return 400, nil, "", true
	}
	if player == nil {
		return 409, nil, "", true
	}
	if seek {
		player.mu.Lock()
		duration := player.duration
		player.mu.Unlock()
		if duration > 0 && value > duration {
			return 400, nil, "", true
		}
	}
	if err := player.command(seek, value); err != nil {
		return 503, nil, "", true
	}
	return 200, nil, "", true
}
