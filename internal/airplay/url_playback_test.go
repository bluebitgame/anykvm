package airplay

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"strings"
	"testing"
	"time"

	"anykvm/internal/avmedia"
	"howett.net/plist"
)

func TestURLPlayParameters(t *testing.T) {
	for _, binary := range []bool{false, true} {
		body := []byte("Content-Location: https://example.com/video.m3u8?token=a:b\r\nStart-Position: 0.25\r\n")
		headers := make(textproto.MIMEHeader)
		if binary {
			body, _ = plist.Marshal(playRequest{Location: "https://example.com/video.m3u8?token=a:b", Start: .25}, plist.BinaryFormat)
			headers.Set("Content-Type", "application/x-apple-binary-plist")
		}
		p, err := parsePlay(request{body: body, headers: headers})
		if err != nil || p.Start != .25 || !strings.HasSuffix(p.Location, "token=a:b") {
			t.Fatalf("%+v %v", p, err)
		}
	}
	for _, body := range []string{"Content-Location: file:///etc/passwd", "Content-Location: https://example.com/a\nStart-Position: NaN", "Content-Location: http://example.com/a\nStart-Position: 1.1", "Content-Location: http://user:pass@example.com/a", "Content-Location: http://example.com/a\nContent-Location: http://example.com/b"} {
		if _, err := parsePlay(request{body: []byte(body)}); err == nil {
			t.Fatalf("accepted %q", body)
		}
	}
	q, err := readRequest(bufio.NewReader(strings.NewReader("POST /scrub?position=1.25 HTTP/1.1\r\n\r\n")))
	if err != nil || q.query.Get("position") != "1.25" {
		t.Fatalf("lost seek query: %+v %v", q, err)
	}
}
func mediaReceiver(t *testing.T) (*Receiver, string) {
	t.Helper()
	if os.Getenv("ANYKVM_TEST_AVMEDIA") == "" {
		t.Skip("set ANYKVM_TEST_AVMEDIA and ANYKVM_FFMPEG_LIBDIR")
	}
	if err := avmedia.URLMediaAvailable(); err != nil {
		t.Fatal(err)
	}
	r, err := NewReceiver("airplay:URL-Test", Options{})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.start(l, nil)
	t.Cleanup(func() { r.Close() })
	return r, "http://" + l.Addr().String()
}
func mediaControl(t *testing.T, address, method, path, session string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, address+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Apple-Session-ID", session)
	req.Header.Set("Content-Type", "text/parameters")
	client := http.Client{Timeout: 5 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, data
}
func waitURLState(t *testing.T, r *Receiver, state string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		p := r.urlPlayer
		r.mu.Unlock()
		if p != nil {
			p.mu.Lock()
			s := p.state
			p.mu.Unlock()
			if s == state {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("missing state %s", state)
}
func TestURLPlaybackControlsAndOwnership(t *testing.T) {
	r, address := mediaReceiver(t)
	media := httptest.NewServer(http.FileServer(http.Dir("../testdata/url")))
	defer media.Close()
	audio := &audioDevice{r}
	if err := audio.Open(); err != nil {
		t.Fatal(err)
	}
	play := []byte("Content-Location: " + media.URL + "/movie.mp4\r\nStart-Position: 0\r\n")
	if status, _ := mediaControl(t, address, "POST", "/play", "one", play); status != 200 {
		t.Fatal(status)
	}
	waitURLState(t, r, "playing")
	// The first IDR and following frames fill the queue before a viewer attaches.
	time.Sleep(250 * time.Millisecond)
	if status, _ := mediaControl(t, address, "POST", "/rate?value=0", "one", nil); status != 200 {
		t.Fatal(status)
	}
	waitURLState(t, r, "paused")
	if status, _ := mediaControl(t, address, "POST", "/stop", "other", nil); status != 453 {
		t.Fatal("foreign stop", status)
	}
	if status, _ := mediaControl(t, address, "POST", "/rate?value=NaN", "one", nil); status != 400 {
		t.Fatal("invalid rate", status)
	}
	if status, _ := mediaControl(t, address, "POST", "/scrub?position=1.1", "one", nil); status != 200 {
		t.Fatal("paused seek", status)
	}
	if len(r.frames) != 0 || len(r.audioFrames) != 0 {
		t.Fatal("seek retained old media")
	}
	_, body := mediaControl(t, address, "GET", "/playback-info", "one", nil)
	var info struct {
		Position float64 `plist:"position"`
		Rate     float64 `plist:"rate"`
		Ready    bool    `plist:"readyToPlay"`
	}
	if _, err := plist.Unmarshal(body, &info); err != nil || info.Position != 1.1 || info.Rate != 0 || !info.Ready {
		t.Fatalf("playback info %+v %v", info, err)
	}
	if status, _ := mediaControl(t, address, "POST", "/rate?value=1", "one", nil); status != 200 {
		t.Fatal(status)
	}
	frame, err := r.ReadFrame()
	if err != nil || frame == nil {
		t.Fatalf("no URL video: %v", err)
	}
	frame.Release()
	if status, _ := mediaControl(t, address, "POST", "/stop", "one", nil); status != 200 {
		t.Fatal(status)
	}
	if len(r.frames) != 0 || len(r.audioFrames) != 0 || audio.AudioAvailable() {
		t.Fatal("stop retained media ownership")
	}
	// A new session and HLS playback must work after stop, without restarting mDNS.
	play = []byte("Content-Location: " + media.URL + "/master.m3u8\r\n")
	if status, _ := mediaControl(t, address, "POST", "/play", "two", play); status != 200 {
		t.Fatal(status)
	}
	waitURLState(t, r, "playing")
	frame, err = r.ReadFrame()
	if err != nil || frame == nil {
		t.Fatalf("no HLS video: %v", err)
	}
	frame.Release()
	r.mu.Lock()
	p := r.urlPlayer
	r.mu.Unlock()
	r.Close()
	select {
	case <-p.done:
	default:
		t.Fatal("receiver Close left media worker")
	}
}
func TestURLPlaybackReverseEvents(t *testing.T) {
	r, address := mediaReceiver(t)
	conn, err := net.Dial("tcp", strings.TrimPrefix(address, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(conn, "POST /reverse HTTP/1.1\r\nUpgrade: PTTH/1.0\r\nConnection: Upgrade\r\nX-Apple-Session-ID: event-session\r\n\r\n")
	reader := bufio.NewReader(conn)
	res, err := http.ReadResponse(reader, nil)
	if err != nil || res.StatusCode != 101 {
		t.Fatalf("reverse %v %v", res, err)
	}
	readEvent := func(want string) {
		t.Helper()
		req, err := http.ReadRequest(reader)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(req.Body)
		req.Body.Close()
		var event struct {
			State string `plist:"state"`
		}
		plist.Unmarshal(body, &event)
		if req.URL.Path != "/event" || req.Header.Get("X-Apple-Session-ID") != "event-session" || event.State != want {
			t.Fatalf("event %s want %s", event.State, want)
		}
		io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
	}
	media := httptest.NewServer(http.FileServer(http.Dir("../testdata/url")))
	defer media.Close()
	body := []byte("Content-Location: " + media.URL + "/movie.mp4\r\n")
	if status, _ := mediaControl(t, address, "POST", "/play", "event-session", body); status != 200 {
		t.Fatal(status)
	}
	readEvent("loading")
	readEvent("playing")
	if status, _ := mediaControl(t, address, "POST", "/rate?value=0", "event-session", nil); status != 200 {
		t.Fatal(status)
	}
	readEvent("paused")
	if status, _ := mediaControl(t, address, "POST", "/stop", "event-session", nil); status != 200 {
		t.Fatal(status)
	}
	readEvent("stopped")
	r.Close()
}

func TestURLPlaybackShutdownCancelsBlockedControl(t *testing.T) {
	r, address := mediaReceiver(t)
	entered := make(chan struct{}, 1)
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { entered <- struct{}{}; <-q.Context().Done() }))
	defer media.Close()
	body := []byte("Content-Location: " + media.URL + "/blocked.mp4\r\n")
	if status, _ := mediaControl(t, address, "POST", "/play", "blocked", body); status != 200 {
		t.Fatal(status)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("media open not started")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequest("POST", address+"/rate?value=0", nil)
		req.Header.Set("X-Apple-Session-ID", "blocked")
		client := http.Client{Timeout: 3 * time.Second}
		res, _ := client.Do(req)
		if res != nil {
			res.Body.Close()
		}
	}()
	deadline := time.Now().Add(time.Second)
	for r.urlMu.TryLock() {
		r.urlMu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("control did not wait for open")
		}
		time.Sleep(time.Millisecond)
	}
	started := time.Now()
	r.Close()
	if time.Since(started) > 2*time.Second {
		t.Fatal("shutdown waited on blocked HTTP control")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("HTTP control leaked")
	}
}
func TestURLPlaybackDeliversBothQueuesAndEOF(t *testing.T) {
	r, address := mediaReceiver(t)
	media := httptest.NewServer(http.FileServer(http.Dir("../testdata/url")))
	defer media.Close()
	audio := &audioDevice{r}
	if err := audio.Open(); err != nil {
		t.Fatal(err)
	}
	body, _ := plist.Marshal(playRequest{Location: media.URL + "/movie.m3u8"}, plist.BinaryFormat)
	if status, _ := mediaControl(t, address, "POST", "/play", "queues", body); status != 200 {
		t.Fatal(status)
	}
	audioDone := make(chan int, 1)
	quit := make(chan struct{})
	defer close(quit)
	go func() {
		count := 0
		for {
			select {
			case <-quit:
				audioDone <- count
				return
			default:
			}
			f, err := audio.Capture()
			if err != nil {
				audioDone <- count
				return
			}
			if f != nil && len(f.Encoded) > 0 && len(f.Config) > 0 {
				count++
			}
		}
	}()
	pictures := 0
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f, err := r.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if f != nil {
			pictures++
			f.Release()
		}
		r.mu.Lock()
		p := r.urlPlayer
		r.mu.Unlock()
		if p != nil {
			p.mu.Lock()
			ended := p.state == "stopped"
			p.mu.Unlock()
			if ended && len(r.frames) == 0 {
				break
			}
		}
	}
	if pictures != 20 {
		t.Fatalf("URL queue delivered %d video packets", pictures)
	}
	waitURLState(t, r, "stopped")
	// EOF keeps the demuxer available for a new seek, until explicit stop.
	if status, _ := mediaControl(t, address, "POST", "/scrub?position=0", "queues", nil); status != 200 {
		t.Fatal("seek after EOF", status)
	}
	if status, _ := mediaControl(t, address, "POST", "/stop", "queues", nil); status != 200 {
		t.Fatal(status)
	}
	r.Close()
	select {
	case count := <-audioDone:
		if count < 80 {
			t.Fatalf("URL queue delivered %d AAC packets", count)
		}
	case <-time.After(time.Second):
		t.Fatal("audio reader leaked")
	}
}

func TestURLDiscoveryCapabilityGate(t *testing.T) {
	base := uint64(1<<7 | 1<<8 | 1<<9 | 1<<14 | 1<<18 | 1<<19 | 1<<20 | 1<<21 | 1<<27 | 1<<30)
	for _, enabled := range []bool{false, true} {
		r := &Receiver{urlMedia: enabled}
		want := base
		if enabled {
			want |= 1<<0 | 1<<4
		}
		if r.featureBits() != want {
			t.Fatalf("URL capability %t: features=%x want=%x", enabled, r.featureBits(), want)
		}
	}
}
