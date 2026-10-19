package airplay

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"anykvm/internal/capture"
	"anykvm/internal/wsmedia"
	"howett.net/plist"
)

type reversePeerConn struct {
	net.Conn
	peer net.Addr
}

func (c reversePeerConn) RemoteAddr() net.Addr { return c.peer }

func rejectedReverse(t *testing.T, r *Receiver, peer, session string) {
	t.Helper()
	server, client := net.Pipe()
	defer client.Close()
	defer server.Close()
	client.SetDeadline(time.Now().Add(time.Second))
	conn := reversePeerConn{Conn: server, peer: &net.TCPAddr{IP: net.ParseIP(peer), Port: 12345}}
	q := request{method: "POST", path: "/reverse", protocol: "HTTP/1.1", headers: make(textproto.MIMEHeader)}
	q.headers.Set("Upgrade", "PTTH/1.0")
	q.headers.Set("Connection", "Upgrade")
	q.headers.Set("X-Apple-Session-ID", session)
	done := make(chan struct{})
	go func() { defer close(done); r.serveReverse(q, conn, bufio.NewReader(conn)) }()
	response, err := http.ReadResponse(bufio.NewReader(client), nil)
	client.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("rejected reverse leaked a worker")
	}
	if err != nil || response.StatusCode != 453 {
		t.Fatalf("foreign reverse accepted: %v %v", response, err)
	}
}

func TestBrowserMediaTransitionsFromPairedRTP(t *testing.T) {
	for _, streams := range [][]uint64{nil, {96}, {110}, {96, 110}} {
		t.Run(fmt.Sprint(streams), func(t *testing.T) {
			d, address := browserReceiver(t)
			conn, err := net.Dial("tcp", strings.TrimPrefix(address, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			sender := &testSender{conn: conn, reader: bufio.NewReader(conn)}
			sender.pair(t)
			sender.fairplay(t)
			timing, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
			if err != nil {
				t.Fatal(err)
			}
			defer timing.Close()
			ek := make([]byte, 72)
			for i := range ek {
				ek[i] = byte(255 - i)
			}
			body, _ := plist.Marshal(map[string]any{"ekey": ek, "eiv": make([]byte, 16), "timingPort": uint64(timing.LocalAddr().(*net.UDPAddr).Port)}, plist.BinaryFormat)
			if status, _ := sender.exchange(t, "SETUP", "/stream", body); status != 200 {
				t.Fatal("initial RTP setup", status)
			}
			var streamList []any
			for _, kind := range streams {
				streamList = append(streamList, map[string]any{"type": kind, "ct": uint64(2), "streamConnectionID": uint64(123)})
			}
			if len(streamList) > 0 {
				body, _ = plist.Marshal(map[string]any{"streams": streamList}, plist.BinaryFormat)
				if status, _ := sender.exchange(t, "SETUP", "/stream", body); status != 200 {
					t.Fatal("RTP streams", status)
				}
			}
			play := []byte("Content-Location: mlhls://localhost/master.m3u8")
			foreign := &idleControlConn{peer: net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 12345}}
			q := request{method: "POST", path: "/play", headers: make(textproto.MIMEHeader), body: play}
			q.headers.Set("X-Apple-Session-ID", "video")
			if code, _, _, _ := d.receiver.handleURL(q, foreign); code != 453 {
				t.Fatal("foreign sender displaced RTP", code)
			}
			rejectedReverse(t, d.receiver, "192.0.2.1", "video")
			startFCUPSender(t, address, "video", func(q fcupTestRequest) error {
				code, err := fcupAction(address, "video", q, 200, []byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nvideo.m3u8\n"))
				if err == nil && code != 200 {
					return fmt.Errorf("FCUP reply %d", code)
				}
				return err
			})
			if code, _ := mediaControl(t, address, "POST", "/play", "video", play); code != 200 {
				t.Fatal("video transition", code)
			}
			state := d.BrowserMediaState()
			if state == nil {
				t.Fatal("missing Web video state")
			}
			rejectedReverse(t, d.receiver, "127.0.0.1", "other-session")
			// The phone may keep its original RTP control and send a delayed
			// teardown after /play. It must not cancel the new video session.
			if code, _ := sender.exchange(t, "TEARDOWN", "/stream", nil); code != 200 {
				t.Fatal("old RTP teardown", code)
			}
			playlist := httptest.NewRecorder()
			d.ServeBrowserMedia(playlist, httptest.NewRequest("GET", state.URL, nil))
			if playlist.Code != 200 || !strings.Contains(playlist.Body.String(), "/airplay/media/"+state.ID+"/") {
				t.Fatal("FCUP after RTP teardown", playlist.Code)
			}
		})
	}
}

// The Web container path deliberately uses no avmedia availability check.
func browserReceiver(t *testing.T) (*Device, string) {
	t.Helper()
	d, err := NewDevice("airplay:Browser-Test", Options{})
	if err != nil {
		t.Fatal(err)
	}
	d.EnableBrowserMedia()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d.receiver.start(listener, nil)
	t.Cleanup(func() { d.receiver.Close() })
	if d.receiver.urlMedia {
		t.Fatal("browser route enabled server demux")
	}
	return d, "http://" + listener.Addr().String()
}

func browserGet(t *testing.T, server, path, method, span string) (int, []byte, http.Header) {
	t.Helper()
	q, err := http.NewRequest(method, server+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	q.Header.Set("Range", span)
	client := http.Client{Timeout: 3 * time.Second}
	res, err := client.Do(q)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, data, res.Header
}

func TestBrowserMediaControlsAndProxy(t *testing.T) {
	d, address := browserReceiver(t)
	files := httptest.NewServer(http.FileServer(http.Dir("../testdata/url")))
	defer files.Close()
	web := httptest.NewServer(http.HandlerFunc(d.ServeBrowserMedia))
	defer web.Close()
	status, _ := mediaControl(t, address, "POST", "/play", "web", []byte("Content-Location: "+files.URL+"/movie.mp4\nStart-Position-Seconds: 0.25"))
	if status != 200 {
		t.Fatalf("play %d", status)
	}
	state := d.BrowserMediaState()
	if state == nil || state.Position != .25 {
		t.Fatalf("state %+v", state)
	}
	code, data, header := browserGet(t, web.URL, state.URL, "GET", "bytes=16-31")
	original, _ := os.ReadFile("../testdata/url/movie.mp4")
	if code != 206 || !bytes.Equal(data, original[16:32]) || header.Get("Content-Range") == "" {
		t.Fatalf("range %d %v", code, header)
	}
	code, data, _ = browserGet(t, web.URL, state.URL, "HEAD", "")
	if code != 200 || len(data) != 0 {
		t.Fatalf("HEAD %d %d", code, len(data))
	}
	for _, forbidden := range []string{"file:///etc/passwd", files.URL + "/unissued.mp4"} {
		path := "/airplay/media/" + state.ID + "/" + base64.RawURLEncoding.EncodeToString([]byte(forbidden))
		if code, _, _ := browserGet(t, web.URL, path, "GET", ""); code != 404 {
			t.Fatalf("unissued URL %d", code)
		}
	}
	feedback := capture.BrowserMediaFeedback{ID: state.ID, Version: state.Version, Duration: 2, Position: .8, Ready: true, State: "playing", Buffered: []capture.MediaTimeRange{{Start: 0, Duration: 1}}, Seekable: []capture.MediaTimeRange{{Start: 0, Duration: 2}}}
	if !d.BrowserMediaFeedback(feedback) {
		t.Fatal("feedback refused")
	}
	_, body := mediaControl(t, address, "GET", "/playback-info", "web", nil)
	var info struct {
		Position float64 `plist:"position"`
		Loaded   []struct {
			Duration float64 `plist:"duration"`
		} `plist:"loadedTimeRanges"`
	}
	if _, err := plist.Unmarshal(body, &info); err != nil || info.Position != .8 || len(info.Loaded) != 1 || info.Loaded[0].Duration != 1 {
		t.Fatalf("info %+v %v", info, err)
	}
	if status, _ := mediaControl(t, address, "POST", "/rate?value=0", "web", nil); status != 200 {
		t.Fatal(status)
	}
	paused := d.BrowserMediaState()
	if !paused.Paused || paused.Position != .8 || paused.Version <= state.Version {
		t.Fatalf("pause rewound %+v", paused)
	}
	if d.BrowserMediaFeedback(feedback) {
		t.Fatal("accepted feedback before pause command")
	}
	if status, _ := mediaControl(t, address, "POST", "/scrub?position=1.1", "web", nil); status != 200 {
		t.Fatal(status)
	}
	seek := d.BrowserMediaState()
	if !seek.Paused || seek.Position != 1.1 || seek.StartFraction != 0 {
		t.Fatalf("seek %+v", seek)
	}
	feedback.Version = seek.Version
	feedback.Position = 1.1
	feedback.State = "paused"
	if !d.BrowserMediaFeedback(feedback) {
		t.Fatal("seek feedback refused")
	}
	feedback.Position = math.NaN()
	if d.BrowserMediaFeedback(feedback) {
		t.Fatal("accepted invalid time")
	}
	if status, _ := mediaControl(t, address, "POST", "/rate?value=1", "other", nil); status != 453 {
		t.Fatal("foreign control accepted", status)
	}
	select {
	case <-d.receiver.frames:
		t.Fatal("URL playback leaked a mirror frame")
	default:
	}
	mediaControl(t, address, "POST", "/stop", "web", nil)
	if d.BrowserMediaState() != nil {
		t.Fatal("player survived stop")
	}
	if code, _, _ := browserGet(t, web.URL, state.URL, "GET", ""); code != 404 {
		t.Fatal("old token survived", code)
	}
}

func TestBrowserMediaHLSAndFCUP(t *testing.T) {
	for _, virtual := range []bool{false, true} {
		name := "HTTP"
		if virtual {
			name = "FCUP"
		}
		t.Run(name, func(t *testing.T) {
			d, address := browserReceiver(t)
			cdn := httptest.NewServer(http.FileServer(http.Dir("../testdata/url")))
			defer cdn.Close()
			web := httptest.NewServer(http.HandlerFunc(d.ServeBrowserMedia))
			defer web.Close()
			location := cdn.URL + "/master.m3u8"
			if virtual {
				location = "mlhls://localhost/master.m3u8"
				startFCUPSender(t, address, "web", func(q fcupTestRequest) error {
					var data []byte
					if q.URL == location {
						data = []byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,CODECS=\"avc1.64001f,mp4a.40.2\"\n" + cdn.URL + "/fragmented.m3u8\n")
					} else {
						t.Error("unexpected virtual URI")
						return nil
					}
					status, err := fcupAction(address, "web", q, 200, data)
					if status != 200 && err == nil {
						t.Errorf("action %d", status)
					}
					return err
				})
			}
			if status, _ := mediaControl(t, address, "POST", "/play", "web", []byte("Content-Location: "+location)); status != 200 {
				t.Fatal(status)
			}
			state := d.BrowserMediaState()
			status, list, header := browserGet(t, web.URL, state.URL, "GET", "")
			if status != 200 || header.Get("Content-Type") != "application/vnd.apple.mpegurl" {
				t.Fatal("master", status, header)
			}
			var child string
			for _, line := range strings.Split(string(list), "\n") {
				if line != "" && !strings.HasPrefix(line, "#") {
					child = line
					break
				}
			}
			if !strings.HasPrefix(child, "/airplay/media/"+state.ID+"/") {
				t.Fatalf("unproxied media URI %q", child)
			}
			status, list, _ = browserGet(t, web.URL, child, "GET", "")
			if status != 200 {
				t.Fatal("child", status)
			}
			for _, line := range strings.Split(string(list), "\n") {
				if line != "" && !strings.HasPrefix(line, "#") {
					status, data, _ := browserGet(t, web.URL, line, "GET", "")
					if status != 200 || len(data) == 0 {
						t.Fatal("segment", status, len(data))
					}
				} else if strings.HasPrefix(line, "#EXT-X-MAP:") {
					attrs, _ := playlistAttributes(line)
					status, data, _ := browserGet(t, web.URL, attrs["URI"], "GET", "bytes=0-15")
					if status != 206 || len(data) != 16 {
						t.Fatal("init", status, len(data))
					}
				}
			}
			mediaControl(t, address, "POST", "/stop", "web", nil)
		})
	}
}

func TestDeviceURLPlaybackRequiresWeb(t *testing.T) {
	d, err := NewDevice("airplay:Mirror-Only", Options{})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d.receiver.start(listener, nil)
	defer d.receiver.Close()
	if d.receiver.featureBits() != features {
		t.Fatal("mirror-only advertised media playback")
	}
	if status, _ := mediaControl(t, "http://"+listener.Addr().String(), "POST", "/play", "", []byte("Content-Location: https://example.com/movie.mp4")); status != 503 {
		t.Fatal(status)
	}
}

func TestBrowserResourceCancellation(t *testing.T) {
	d, address := browserReceiver(t)
	requested, canceled := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(requested)
		<-q.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	web := httptest.NewServer(http.HandlerFunc(d.ServeBrowserMedia))
	defer web.Close()
	mediaControl(t, address, "POST", "/play", "web", []byte("Content-Location: "+upstream.URL+"/blocked.mp4"))
	state := d.BrowserMediaState()
	done := make(chan struct{})
	go func() {
		defer close(done)
		res, err := http.Get(web.URL + state.URL)
		if err == nil {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
	}()
	select {
	case <-requested:
	case <-time.After(time.Second):
		t.Fatal("upstream request did not start")
	}
	mediaControl(t, address, "POST", "/stop", "web", nil)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel upstream")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("browser request leaked")
	}
}

// Optional real-browser regression: platform decoding, MPEG-TS transmuxing,
// fMP4, FCUP and AirPlay controls. No server demuxer participates.
func TestBrowserContainerE2E(t *testing.T) {
	node := os.Getenv("ANYKVM_TEST_BROWSER_NODE")
	if node == "" {
		t.Skip("set ANYKVM_TEST_BROWSER_NODE, ANYKVM_TEST_PLAYWRIGHT and ANYKVM_TEST_BROWSER_CHROME")
	}
	d, address := browserReceiver(t)
	cdn := httptest.NewServer(http.FileServer(http.Dir("../testdata/url")))
	defer cdn.Close()
	startFCUPSender(t, address, "browser", func(q fcupTestRequest) error {
		name := strings.TrimPrefix(q.URL, "mlhls://localhost/")
		if name != "fragmented.m3u8" && name != "init.mp4" && name != "fragment0.m4s" && name != "fragment1.m4s" {
			return fmt.Errorf("unexpected FCUP URI")
		}
		data, err := os.ReadFile("../testdata/url/" + name)
		if err != nil {
			return err
		}
		status, err := fcupAction(address, "browser", q, 200, data)
		// hls.js may cancel an in-flight fragment during a seek. Its late
		// sender reply is rejected, but the sender must keep serving new IDs.
		if err == nil && status != 200 && status != 409 {
			return fmt.Errorf("FCUP status %d", status)
		}
		return err
	})
	viewer := wsmedia.NewServer(wsmedia.Config{BrowserMedia: d})
	defer viewer.Close()
	handler, err := viewer.Handler()
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(handler)
	defer web.Close()
	cmd := exec.Command(node, "../wsmedia/web_test/media.browser.mjs", web.URL, address, cdn.URL)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("browser regression %v\n%s", err, output)
	}
	t.Log(string(output))
}
