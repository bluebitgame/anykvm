package airplay

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"anykvm/internal/capture"
)

type browserPlayback struct {
	id, url            string
	version            uint64 // Desired controls; feedback from older controls is ignored.
	seekVersion        uint64
	paused             bool
	position, fraction float64
	buffered, seekable []capture.MediaTimeRange
	rewriter           *fcupStore
	client             *http.Client
	resourcesMu        sync.Mutex
	resources          map[string]uint64
	sequence           uint64
}

func (p *urlPlayer) initBrowser(request playRequest) error {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return err
	}
	u, _ := url.Parse(request.Location)
	b := &browserPlayback{id: hex.EncodeToString(token[:]), version: 1, seekVersion: 1, fraction: request.Start, resources: make(map[string]uint64)}
	if request.Seconds != nil {
		b.position, b.fraction = *request.Seconds, 0
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 10 * time.Second
	transport.MaxResponseHeaderBytes = 32 << 10
	b.client = &http.Client{Transport: transport, CheckRedirect: func(q *http.Request, via []*http.Request) error {
		if len(via) >= 10 || !validHTTPMediaURL(q.URL) {
			return fmt.Errorf("invalid media redirect")
		}
		return nil
	}}
	if p.fcup != nil {
		b.rewriter = p.fcup
	} else {
		b.rewriter = newFCUPStore(p.ctx, p.receiver, p.key, u)
	}
	b.rewriter.mapURL = b.resourceURL
	b.url = b.resourceURL(request.Location)
	p.browser = b
	p.position = b.position
	return nil
}

func validHTTPMediaURL(u *url.URL) bool {
	return u != nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil && u.Fragment == "" && len(u.String()) <= 16384 && !strings.ContainsAny(u.String(), "\r\n\x00")
}

// Only URLs issued by /play or a validated playlist can be fetched. Knowing a
// playback token must not turn the authenticated Web endpoint into an open proxy.
func (b *browserPlayback) resourceURL(uri string) string {
	b.resourcesMu.Lock()
	defer b.resourcesMu.Unlock()
	b.sequence++
	if _, ok := b.resources[uri]; !ok && len(b.resources) >= 4096 {
		var oldest string
		var seq uint64
		for key, stamp := range b.resources {
			if oldest == "" || stamp < seq {
				oldest, seq = key, stamp
			}
		}
		delete(b.resources, oldest)
	}
	b.resources[uri] = b.sequence
	return "/airplay/media/" + b.id + "/" + base64.RawURLEncoding.EncodeToString([]byte(uri))
}

func (p *urlPlayer) runBrowser() {
	defer close(p.done)
	defer p.setState("stopped")
	defer p.browser.rewriter.close()
	defer p.browser.client.CloseIdleConnections()
	<-p.ctx.Done()
}

func (d *Device) BrowserMediaState() *capture.BrowserMediaState {
	r := d.receiver
	r.mu.Lock()
	p := r.urlPlayer
	r.mu.Unlock()
	if p == nil || p.browser == nil || p.ctx.Err() != nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	b := p.browser
	return &capture.BrowserMediaState{ID: b.id, URL: b.url, Version: b.version, SeekVersion: b.seekVersion, Paused: b.paused, Position: b.position, StartFraction: b.fraction}
}

func finiteMediaTime(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v < float64(math.MaxInt64)/1e6
}
func validMediaRanges(ranges []capture.MediaTimeRange) bool {
	if len(ranges) > 32 {
		return false
	}
	for _, span := range ranges {
		if !finiteMediaTime(span.Start) || !finiteMediaTime(span.Duration) || !finiteMediaTime(span.Start+span.Duration) {
			return false
		}
	}
	return true
}
func playbackRanges(ranges []capture.MediaTimeRange) []any {
	out := make([]any, 0, len(ranges))
	for _, span := range ranges {
		out = append(out, map[string]any{"start": span.Start, "duration": span.Duration})
	}
	return out
}

func (d *Device) BrowserMediaFeedback(f capture.BrowserMediaFeedback) bool {
	if !finiteMediaTime(f.Duration) || !finiteMediaTime(f.Position) || !validMediaRanges(f.Buffered) || !validMediaRanges(f.Seekable) {
		return false
	}
	if f.State != "loading" && f.State != "playing" && f.State != "paused" && f.State != "stopped" {
		return false
	}
	r := d.receiver
	r.mu.Lock()
	p := r.urlPlayer
	r.mu.Unlock()
	if p == nil || p.browser == nil || p.ctx.Err() != nil {
		return false
	}
	p.mu.Lock()
	if f.ID != p.browser.id || f.Version != p.browser.version {
		p.mu.Unlock()
		return false
	}
	p.duration, p.position, p.ready = f.Duration, f.Position, f.Ready
	p.browser.buffered = append([]capture.MediaTimeRange(nil), f.Buffered...)
	p.browser.seekable = append([]capture.MediaTimeRange(nil), f.Seekable...)
	// Report timeline and state together. sendMediaEvent does not acquire p.mu.
	changed := p.state != f.State
	p.state = f.State
	p.mu.Unlock()
	if changed {
		r.sendMediaEvent(p.key, f.State)
	}
	return true
}

func (d *Device) ServeBrowserMedia(w http.ResponseWriter, q *http.Request) {
	if q.Method != "GET" && q.Method != "HEAD" {
		w.WriteHeader(405)
		return
	}
	r := d.receiver
	r.mu.Lock()
	p := r.urlPlayer
	r.mu.Unlock()
	if p == nil || p.browser == nil || p.ctx.Err() != nil {
		http.NotFound(w, q)
		return
	}
	b := p.browser
	prefix := "/airplay/media/" + b.id + "/"
	if !strings.HasPrefix(q.URL.Path, prefix) || len(q.URL.Path) > 24000 || q.URL.RawQuery != "" {
		http.NotFound(w, q)
		return
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(q.URL.Path, prefix))
	if err != nil {
		http.NotFound(w, q)
		return
	}
	uri := string(data)
	b.resourcesMu.Lock()
	_, issued := b.resources[uri]
	if issued {
		b.sequence++
		b.resources[uri] = b.sequence
	}
	b.resourcesMu.Unlock()
	if !issued {
		http.NotFound(w, q)
		return
	}
	ctx, cancel := context.WithCancel(q.Context())
	stopCancel := context.AfterFunc(p.ctx, cancel)
	defer func() { stopCancel(); cancel() }()
	u, _ := url.Parse(uri)
	if validFCUPURL(u) {
		data, err = p.fcup.fetch(ctx, uri)
		if err != nil {
			http.Error(w, "AirPlay resource unavailable", 503)
			return
		}
		serveBrowserContent(w, q, data)
		return
	}
	if !validHTTPMediaURL(u) {
		http.NotFound(w, q)
		return
	}
	req, err := http.NewRequestWithContext(ctx, "GET", uri, nil)
	if err != nil {
		http.NotFound(w, q)
		return
	}
	req.Header.Set("User-Agent", "AppleCoreMedia/1.0.0.11B554a")
	req.Header.Set("Accept-Encoding", "identity")
	// Range is essential for MP4 and EXT-X-BYTERANGE segments. Playlists are
	// small bounded objects that must be rewritten in their entirety.
	if !strings.HasSuffix(strings.ToLower(u.Path), ".m3u8") {
		req.Header.Set("Range", q.Header.Get("Range"))
		req.Header.Set("If-Range", q.Header.Get("If-Range"))
	}
	res, err := b.client.Do(req)
	if err != nil {
		http.Error(w, "AirPlay upstream unavailable", 502)
		return
	}
	defer res.Body.Close()
	if res.StatusCode != 200 && res.StatusCode != 206 {
		if res.StatusCode == 404 || res.StatusCode == 410 || res.StatusCode == 416 {
			w.WriteHeader(res.StatusCode)
		} else {
			http.Error(w, "AirPlay upstream unavailable", 502)
		}
		return
	}
	reader := bufio.NewReader(res.Body)
	prefixBytes, _ := reader.Peek(7)
	if string(prefixBytes) == "#EXTM3U" {
		if res.StatusCode != 200 {
			http.Error(w, "Partial HLS playlist unavailable", 502)
			return
		}
		data, err = io.ReadAll(io.LimitReader(reader, maxFCUPResource+1))
		if err == nil {
			data, err = b.rewriter.rewritePlaylist(res.Request.URL.String(), data)
		}
		if err != nil {
			http.Error(w, "AirPlay playlist unavailable", 502)
			return
		}
		serveBrowserContent(w, q, data)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	for _, key := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
		if value := res.Header.Get(key); value != "" {
			w.Header().Set(key, value)
		}
	}
	w.WriteHeader(res.StatusCode)
	if q.Method != "HEAD" {
		_, _ = io.Copy(w, reader)
	}
}

func serveBrowserContent(w http.ResponseWriter, q *http.Request, data []byte) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/octet-stream")
	if strings.HasPrefix(string(data), "#EXTM3U") {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	}
	http.ServeContent(w, q, "", time.Time{}, strings.NewReader(string(data)))
}

var _ capture.BrowserMediaSource = (*Device)(nil)
