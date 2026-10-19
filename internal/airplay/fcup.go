package airplay

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"howett.net/plist"
)

const maxFCUPResource = 4 << 20
const maxFCUPCache = 16 << 20
const fcupTimeout = 10 * time.Second

// The localhost in an FCUP URL identifies the sender's resource namespace.
// It must never be interpreted as a receiver-side network destination.
func validFCUPURL(u *url.URL) bool {
	return u != nil && (u.Scheme == "mlhls" || u.Scheme == "nfhls") && u.Host == "localhost" && u.User == nil && u.Fragment == "" && u.Path != "" && len(u.String()) <= 16384 && !strings.ContainsAny(u.String(), "\r\n\x00")
}

type fcupReply struct {
	data []byte
	err  error
}
type fcupPending struct {
	id        uint64
	uri       string
	reply     chan fcupReply
	delivered bool
}
type fcupCached struct {
	data          []byte
	expires, used time.Time
}
type fcupStore struct {
	ctx        context.Context
	cancel     context.CancelFunc
	receiver   *Receiver
	key        playbackKey
	origin     *url.URL
	serial     chan struct{}
	mu         sync.Mutex
	pending    *fcupPending
	cache      map[string]fcupCached
	bytes      int
	base       string // Initialized before the loopback HTTP server starts.
	server     *http.Server
	serverDone chan struct{}
	mapURL     func(string) string // Web maps virtual and CDN resources to one origin.
}

func newFCUPStore(ctx context.Context, r *Receiver, key playbackKey, origin *url.URL) *fcupStore {
	ctx, cancel := context.WithCancel(ctx)
	return &fcupStore{ctx: ctx, cancel: cancel, receiver: r, key: key, origin: origin, serial: make(chan struct{}, 1), cache: make(map[string]fcupCached)}
}

func (s *fcupStore) open(uri string) (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	s.base = "http://" + l.Addr().String() + "/" + hex.EncodeToString(token[:]) + "/"
	s.server = &http.Server{Handler: http.HandlerFunc(s.serve), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 32 << 10, BaseContext: func(net.Listener) context.Context { return s.ctx }}
	s.serverDone = make(chan struct{})
	go func() { defer close(s.serverDone); _ = s.server.Serve(l) }()
	return s.localURL(uri), nil
}
func (s *fcupStore) close() {
	s.cancel()
	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if s.server.Shutdown(ctx) != nil {
			s.server.Close()
		}
		<-s.serverDone
	}
}
func (s *fcupStore) localURL(uri string) string {
	if s.mapURL != nil {
		return s.mapURL(uri)
	}
	return s.base + base64.RawURLEncoding.EncodeToString([]byte(uri))
}
func (s *fcupStore) serve(w http.ResponseWriter, q *http.Request) {
	if q.Method != "GET" && q.Method != "HEAD" {
		w.WriteHeader(405)
		return
	}
	// Compare only the fixed token path, never trust the request Host header.
	u, _ := url.Parse(s.base)
	prefix := u.Path
	if !strings.HasPrefix(q.URL.Path, prefix) || q.URL.RawQuery != "" || len(q.URL.Path) > 24000 {
		http.NotFound(w, q)
		return
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(q.URL.Path, prefix))
	if err != nil {
		http.NotFound(w, q)
		return
	}
	resource, err := url.Parse(string(data))
	if err != nil || !validFCUPURL(resource) || resource.Scheme != s.origin.Scheme {
		http.NotFound(w, q)
		return
	}
	data, err = s.fetch(q.Context(), resource.String())
	if err != nil {
		if s.ctx.Err() == nil && q.Context().Err() == nil {
			log.Printf("airplay: FCUP resource failed: %v", err)
		}
		http.Error(w, "AirPlay resource unavailable", 503)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/octet-stream")
	if strings.HasPrefix(string(data), "#EXTM3U") {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	}
	http.ServeContent(w, q, "", time.Time{}, bytes.NewReader(data))
}

// Serial requests match sender implementations that allow only one outstanding
// FCUP transaction. /action intentionally does not acquire this semaphore or
// the URL control lock, since the media worker may be waiting for its reply.
func (s *fcupStore) fetch(ctx context.Context, uri string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, fcupTimeout)
	defer cancel()
	select {
	case s.serial <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
	defer func() { <-s.serial }()
	s.mu.Lock()
	cached, ok := s.cache[uri]
	if ok && (cached.expires.IsZero() || time.Now().Before(cached.expires)) {
		cached.used = time.Now()
		s.cache[uri] = cached
		s.mu.Unlock()
		return cached.data, nil
	}
	s.mu.Unlock()
	var reverse *reverseEvents
	// Senders can establish /reverse immediately after /play.
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for reverse == nil {
		s.receiver.mu.Lock()
		e := s.receiver.urlEvents
		if e != nil && e.key == s.key {
			reverse = e
		}
		s.receiver.mu.Unlock()
		if reverse != nil {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.ctx.Done():
			return nil, s.ctx.Err()
		}
	}
	s.receiver.mu.Lock()
	s.receiver.fcupNextID++
	id := s.receiver.fcupNextID
	s.receiver.mu.Unlock()
	pending := &fcupPending{id: id, uri: uri, reply: make(chan fcupReply, 1)}
	s.mu.Lock()
	s.pending = pending
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.pending == pending {
			s.pending = nil
		}
		s.mu.Unlock()
	}()
	body, err := plist.Marshal(map[string]any{
		"sessionID": uint64(1), "type": "unhandledURLRequest",
		"request": map[string]any{"sessionID": uint64(1), "FCUP_Response_ClientInfo": uint64(1), "FCUP_Response_ClientRef": uint64(40030004), "FCUP_Response_RequestID": id, "FCUP_Response_URL": uri,
			"FCUP_Response_Headers": map[string]any{"X-Playback-Session-Id": s.key.session, "User-Agent": "AppleCoreMedia/1.0.0.11B554a (Apple TV; U; CPU OS 7_0_4 like Mac OS X; en_us)"}},
	}, plist.XMLFormat)
	if err != nil {
		return nil, err
	}
	select {
	case reverse.requests <- reverseRequest{ctx: ctx, body: body}:
	case <-reverse.done:
		return nil, fmt.Errorf("reverse connection closed")
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
	var reply fcupReply
	select {
	case reply = <-pending.reply:
	case <-reverse.done:
		return nil, fmt.Errorf("reverse connection closed")
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
	if reply.err != nil {
		return nil, reply.err
	}
	data := reply.data
	if strings.HasPrefix(string(data), "#EXTM3U") {
		data, err = s.rewritePlaylist(uri, data)
		if err != nil {
			return nil, err
		}
	} else if strings.HasSuffix(s.origin.Path, ".m3u8") && uri == s.origin.String() {
		return nil, fmt.Errorf("FCUP master is not a playlist")
	}
	if len(data) > maxFCUPResource {
		return nil, fmt.Errorf("FCUP resource exceeds budget")
	}
	entry := fcupCached{data: data, used: time.Now()}
	if strings.HasPrefix(string(data), "#EXTM3U") && !strings.Contains(string(data), "#EXT-X-ENDLIST") && !strings.Contains(string(data), "#EXT-X-STREAM-INF:") {
		entry.expires = time.Now().Add(time.Second)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.cache[uri]; ok {
		s.bytes -= len(old.data)
		delete(s.cache, uri)
	}
	for len(s.cache) >= 128 || s.bytes+len(data) > maxFCUPCache {
		var oldest string
		var stamp time.Time
		for key, item := range s.cache {
			if oldest == "" || item.used.Before(stamp) {
				oldest = key
				stamp = item.used
			}
		}
		s.bytes -= len(s.cache[oldest].data)
		delete(s.cache, oldest)
	}
	s.cache[uri] = entry
	s.bytes += len(data)
	return data, nil
}

func (r *Receiver) handleFCUPAction(q request, conn net.Conn) (int, []byte, string, bool) {
	key, err := mediaKey(q, conn)
	if err != nil {
		return 400, nil, "", true
	}
	r.mu.Lock()
	p := r.urlPlayer
	r.mu.Unlock()
	if p == nil {
		return 409, nil, "", true
	}
	if p.key != key {
		return 453, nil, "", true
	}
	if p.fcup == nil || p.fcup.ctx.Err() != nil {
		return 409, nil, "", true
	}
	var action struct {
		Type   string `plist:"type"`
		Params struct {
			ID     uint64 `plist:"FCUP_Response_RequestID"`
			URL    string `plist:"FCUP_Response_URL"`
			Status uint64 `plist:"FCUP_Response_StatusCode"`
			Data   []byte `plist:"FCUP_Response_Data"`
		} `plist:"params"`
	}
	if len(q.body) > maxFCUPActionBody {
		return 400, nil, "", true
	}
	if _, err := plist.Unmarshal(q.body, &action); err != nil {
		return 400, nil, "", true
	}
	if action.Type != "unhandledURLResponse" {
		return 501, nil, "", true
	}
	if action.Params.ID == 0 || action.Params.Status < 100 || action.Params.Status > 599 || len(action.Params.Data) > maxFCUPResource {
		return 400, nil, "", true
	}
	s := p.fcup
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.pending
	if pending == nil || pending.delivered || pending.id != action.Params.ID || pending.uri != action.Params.URL {
		return 409, nil, "", true
	}
	reply := fcupReply{data: action.Params.Data}
	if action.Params.Status != 200 {
		reply.err = fmt.Errorf("FCUP sender status %d", action.Params.Status)
	}
	pending.delivered = true
	pending.reply <- reply
	return 200, nil, "", true
}
