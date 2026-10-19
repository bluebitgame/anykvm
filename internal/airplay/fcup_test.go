package airplay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"howett.net/plist"
)

func TestFCUPPlayParameters(t *testing.T) {
	for _, binary := range []bool{false, true} {
		body := []byte("Content-Location: mlhls://localhost/master.m3u8\r\nStart-Position-Seconds: 12.5\r\n")
		if binary {
			body, _ = plist.Marshal(map[string]any{"Content-Location": "mlhls://localhost/master.m3u8", "Start-Position-Seconds": 12.5}, plist.BinaryFormat)
		}
		p, err := parsePlay(request{body: body})
		if err != nil || p.Seconds == nil || *p.Seconds != 12.5 {
			t.Fatalf("seconds: %+v %v", p, err)
		}
	}
	for _, location := range []string{"mlhls://example.com/master.m3u8", "mlhls://localhost:80/master.m3u8", "mlhls://user@localhost/master.m3u8", "mlhls://localhost/master.m3u8#x", "mlhls://localhost", "file:///tmp/movie.m3u8"} {
		if _, err := parsePlay(request{body: []byte("Content-Location: " + location)}); err == nil {
			t.Fatalf("accepted %s", location)
		}
	}
	for _, position := range []float64{-1, math.NaN(), math.Inf(1), 1e20} {
		body, _ := plist.Marshal(map[string]any{"Content-Location": "nfhls://localhost/master.m3u8", "Start-Position-Seconds": position}, plist.BinaryFormat)
		if _, err := parsePlay(request{body: body}); err == nil {
			t.Fatal("accepted invalid seconds")
		}
	}
}

func TestFCUPPlaylistRewrite(t *testing.T) {
	origin, _ := url.Parse("mlhls://localhost/master.m3u8")
	s := newFCUPStore(context.Background(), nil, playbackKey{}, origin)
	defer s.close()
	s.base = "http://127.0.0.1:1234/token/"
	input := `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="English, original",URI="/audio.m3u8?track=2"
#EXT-X-STREAM-INF:BANDWIDTH=9000000,CODECS="hvc1.1.6.L93.B0,mp4a.40.2"
hevc.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=100000,CODECS="avc1.64001f,mp4a.40.2",AUDIO="a"
video.m3u8?track=1
`
	data, err := s.rewritePlaylist(origin.String(), []byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `CODECS="hvc1.1.6.L93.B0,mp4a.40.2"`) || strings.Contains(string(data), "mlhls://") {
		t.Fatal("codec declaration lost or virtual URI retained")
	}
	for _, uri := range []string{"mlhls://localhost/audio.m3u8?track=2", "mlhls://localhost/hevc.m3u8", "mlhls://localhost/video.m3u8?track=1"} {
		if !strings.Contains(string(data), s.localURL(uri)) {
			t.Fatalf("missing rewritten URI %s", uri)
		}
	}
	media := `#EXTM3U
#EXT-X-MAP:URI="init.mp4",BYTERANGE="123@0"
#EXT-X-KEY:METHOD=AES-128,URI="key.bin"
#EXTINF:1,
https://cdn.example/video.ts?token=a%2Fb&x=1
#EXT-X-ENDLIST
`
	data, err = s.rewritePlaylist("mlhls://localhost/variant/index.m3u8", []byte(media))
	if err != nil || !strings.Contains(string(data), "https://cdn.example/video.ts?token=a%2Fb&x=1") || !strings.Contains(string(data), s.localURL("mlhls://localhost/variant/init.mp4")) || !strings.Contains(string(data), s.localURL("mlhls://localhost/variant/key.bin")) {
		t.Fatalf("media rewrite %s %v", data, err)
	}
	condensed := `#EXTM3U
#YT-EXT-CONDENSED-URL:BASE-URI="https://cdn.example/v",PARAMS="sq,range",PREFIX="s/"
#EXTINF:1,
s/10/0-999
#EXT-X-ENDLIST
`
	data, err = s.rewritePlaylist(origin.String(), []byte(condensed))
	if err != nil || !strings.Contains(string(data), "https://cdn.example/v/sq/10/range/0-999") || strings.Contains(string(data), "#YT-EXT-CONDENSED") {
		t.Fatalf("condensed %s %v", data, err)
	}
	for _, input := range []string{
		"#EXTM3U\n#EXTINF:1,\nfile:///tmp/movie.ts\n",
		"#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"file:///tmp/key\"\n",
		"#EXTM3U\n#EXTINF:1,\nmlhls://elsewhere/segment.ts\n",
		"#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,CODECS=\"hvc1.1.6.L93.B0\"\n",
		"#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,CODECS=\"hvc1.1.6.L93.B0\"\n#EXT-X-STREAM-INF:BANDWIDTH=2\nv.m3u8\n",
		"#EXTM3U\n#YT-EXT-CONDENSED-URL:BASE-URI=\"https://cdn.example/v\",PARAMS=\"sq,range\",PREFIX=\"s/\"\n#EXTINF:1,\ns/10\n",
	} {
		if _, err := s.rewritePlaylist(origin.String(), []byte(input)); err == nil {
			t.Fatal("accepted invalid playlist")
		}
	}
}

func TestFCUPPlaylistPreservesAllCodecs(t *testing.T) {
	// Codec selection belongs to the player. The proxy must retain even formats
	// it cannot decode, including I-frame renditions and audio-only variants.
	for _, codecs := range []string{
		"avc1.64001f,mp4a.40.2", "hvc1.1.6.L93.B0,mp4a.40.2", "hev1.2.4.L153.B0,ec-3",
		"av01.0.08M.08,opus", "vp09.00.10.08,mp4a.40.5", "dvh1.05.06,ac-3",
		"mp4a.40.2", "flac", "future.video,future.audio", "",
	} {
		t.Run(codecs, func(t *testing.T) {
			origin, _ := url.Parse("mlhls://localhost/master.m3u8")
			s := newFCUPStore(context.Background(), nil, playbackKey{}, origin)
			defer s.close()
			s.base = "http://127.0.0.1:1234/token/"
			variant := fmt.Sprintf(`#EXT-X-STREAM-INF:BANDWIDTH=1000,CODECS="%s",VIDEO-RANGE=PQ`, codecs)
			iframe := fmt.Sprintf(`#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=100,CODECS="%s",URI="iframe.m3u8"`, codecs)
			input := "#EXTM3U\n" + variant + "\nvideo.m3u8\n" + iframe
			data, err := s.rewritePlaylist(origin.String(), []byte(input))
			want := "#EXTM3U\n" + variant + "\n" + s.localURL("mlhls://localhost/video.m3u8") + "\n" + strings.Replace(iframe, "iframe.m3u8", s.localURL("mlhls://localhost/iframe.m3u8"), 1) + "\n"
			if err != nil || string(data) != want {
				t.Fatalf("codec renditions changed: %v\n%s", err, data)
			}
		})
	}
}

type fcupTestRequest struct {
	ID  uint64 `plist:"FCUP_Response_RequestID"`
	URL string `plist:"FCUP_Response_URL"`
}

// A simulated phone acknowledges /event on PTTH, then replies independently on
// /action. The callback can deliberately withhold replies to exercise shutdown.
func startFCUPSender(t *testing.T, address, session string, reply func(fcupTestRequest) error) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(address, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(conn, "POST /reverse HTTP/1.1\r\nUpgrade: PTTH/1.0\r\nConnection: Upgrade\r\nX-Apple-Session-ID: %s\r\n\r\n", session)
	res, err := http.ReadResponse(reader, nil)
	if err != nil || res.StatusCode != 101 {
		conn.Close()
		t.Fatalf("reverse %v %v", res, err)
	}
	done := make(chan error, 1)
	go func() {
		for {
			conn.SetDeadline(time.Now().Add(30 * time.Second))
			q, err := http.ReadRequest(reader)
			if err != nil {
				done <- nil
				return
			}
			body, err := io.ReadAll(q.Body)
			q.Body.Close()
			if err != nil {
				done <- err
				return
			}
			var event struct {
				Type    string          `plist:"type"`
				Request fcupTestRequest `plist:"request"`
			}
			if _, err := plist.Unmarshal(body, &event); err != nil {
				done <- err
				return
			}
			if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"); err != nil {
				// The receiver may close /reverse after /stop before the final
				// stopped event has been acknowledged.
				done <- nil
				return
			}
			if event.Type == "unhandledURLRequest" {
				if err := reply(event.Request); err != nil {
					done <- err
					return
				}
			}
		}
	}()
	t.Cleanup(func() {
		conn.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("sender reader leaked")
		}
	})
}

func fcupAction(address, session string, request fcupTestRequest, status uint64, data []byte) (int, error) {
	body, err := plist.Marshal(map[string]any{"type": "unhandledURLResponse", "params": map[string]any{"FCUP_Response_RequestID": request.ID, "FCUP_Response_URL": request.URL, "FCUP_Response_StatusCode": status, "FCUP_Response_Data": data}}, plist.BinaryFormat)
	if err != nil {
		return 0, err
	}
	q, err := http.NewRequest("POST", address+"/action", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	q.Header.Set("X-Apple-Session-ID", session)
	q.Header.Set("Content-Type", "application/x-apple-binary-plist")
	client := http.Client{Timeout: 3 * time.Second}
	res, err := client.Do(q)
	if err != nil {
		return 0, err
	}
	res.Body.Close()
	return res.StatusCode, nil
}

func TestFCUPPlayback(t *testing.T) {
	r, address := mediaReceiver(t)
	audio := &audioDevice{r}
	if err := audio.Open(); err != nil {
		t.Fatal(err)
	}
	var downloads atomic.Int32
	files := http.FileServer(http.Dir("../testdata/url"))
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		downloads.Add(1)
		if name, ok := strings.CutPrefix(q.URL.Path, "/chunk/"); ok {
			q.URL.Path = "/segment" + name + ".ts"
		}
		files.ServeHTTP(w, q)
	}))
	defer cdn.Close()
	media, err := os.ReadFile("../testdata/url/movie.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	media = bytes.ReplaceAll(media, []byte("segment0.ts"), []byte(cdn.URL+"/chunk/0"))
	media = bytes.ReplaceAll(media, []byte("segment1.ts"), []byte(cdn.URL+"/chunk/1"))
	// Real FCUP playlists can exceed the old 64 KiB control-body budget.
	media = append(media, []byte(strings.Repeat("# metadata padding\n", 4000))...)
	var requests atomic.Int32
	startFCUPSender(t, address, "fcup", func(q fcupTestRequest) error {
		requests.Add(1)
		var data []byte
		switch q.URL {
		case "mlhls://localhost/master.m3u8":
			data = []byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,CODECS=\"avc1.64001f,mp4a.40.2\"\nvariant.m3u8?track=main\n")
		case "mlhls://localhost/variant.m3u8?track=main":
			data = media
		default:
			return fmt.Errorf("unexpected resource %s", q.URL)
		}
		status, err := fcupAction(address, "fcup", q, 200, data)
		if err != nil {
			return err
		}
		if status != 200 {
			return fmt.Errorf("action status %d", status)
		}
		return nil
	})
	body, _ := plist.Marshal(map[string]any{"Content-Location": "mlhls://localhost/master.m3u8", "Start-Position-Seconds": 0.25}, plist.BinaryFormat)
	if status, _ := mediaControl(t, address, "POST", "/play", "fcup", body); status != 200 {
		t.Fatal(status)
	}
	waitURLState(t, r, "playing")
	video, err := r.ReadFrame()
	if err != nil || video == nil {
		t.Fatalf("FCUP video %v", err)
	}
	if video.Width != 64 || video.Height != 48 {
		t.Fatal("wrong FCUP video")
	}
	video.Release()
	// TS demuxing may yield a burst of video before the first audio packet;
	// keep consuming the bounded video queue while waiting for audio.
	foundAudio := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		packet, err := audio.Capture()
		if err != nil {
			t.Fatal(err)
		}
		if packet != nil {
			if packet.Codec != "mp4a.40.2" || len(packet.Config) == 0 || len(packet.Encoded) == 0 {
				t.Fatal("invalid FCUP audio")
			}
			foundAudio = true
			break
		}
		f, err := r.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if f != nil {
			f.Release()
		}
	}
	if !foundAudio {
		t.Fatal("FCUP audio missing")
	}
	if status, _ := mediaControl(t, address, "POST", "/rate?value=0", "fcup", nil); status != 200 {
		t.Fatal(status)
	}
	if status, _ := mediaControl(t, address, "POST", "/scrub?position=1.1", "fcup", nil); status != 200 {
		t.Fatal(status)
	}
	_, body = mediaControl(t, address, "GET", "/playback-info", "fcup", nil)
	var info struct {
		Position float64 `plist:"position"`
		Rate     float64 `plist:"rate"`
	}
	if _, err := plist.Unmarshal(body, &info); err != nil || info.Position != 1.1 || info.Rate != 0 {
		t.Fatalf("FCUP info %+v %v", info, err)
	}
	if status, _ := mediaControl(t, address, "POST", "/rate?value=1", "fcup", nil); status != 200 {
		t.Fatal(status)
	}
	video, err = r.ReadFrame()
	if err != nil || video == nil {
		t.Fatalf("FCUP seek video %v", err)
	}
	video.Release()
	if status, _ := mediaControl(t, address, "POST", "/stop", "fcup", nil); status != 200 {
		t.Fatal(status)
	}
	if requests.Load() != 2 || downloads.Load() < 2 {
		t.Fatalf("FCUP requests=%d CDN downloads=%d", requests.Load(), downloads.Load())
	}
	r.Close()
}

func TestFCUPReplyOwnershipAndCancellation(t *testing.T) {
	r, address := mediaReceiver(t)
	requests := make(chan fcupTestRequest, 4)
	startFCUPSender(t, address, "owner", func(q fcupTestRequest) error { requests <- q; return nil })
	play := func() fcupTestRequest {
		if status, _ := mediaControl(t, address, "POST", "/play", "owner", []byte("Content-Location: nfhls://localhost/master.m3u8\r\n")); status != 200 {
			t.Fatal(status)
		}
		select {
		case q := <-requests:
			return q
		case <-time.After(2 * time.Second):
			t.Fatal("FCUP request missing")
			return fcupTestRequest{}
		}
	}
	first := play()
	for _, trial := range []struct {
		request fcupTestRequest
		session string
		want    int
	}{
		{first, "foreign", 453},
		{fcupTestRequest{ID: first.ID + 1, URL: first.URL}, "owner", 409},
		{fcupTestRequest{ID: first.ID, URL: "nfhls://localhost/other.m3u8"}, "owner", 409},
	} {
		if status, err := fcupAction(address, trial.session, trial.request, 200, []byte("#EXTM3U\n")); err != nil || status != trial.want {
			t.Fatalf("mismatched reply %d %v", status, err)
		}
	}
	if status, _ := mediaControl(t, address, "POST", "/stop", "owner", nil); status != 200 {
		t.Fatal(status)
	}
	second := play()
	if second.ID <= first.ID {
		t.Fatal("FCUP ID reused")
	}
	if status, err := fcupAction(address, "owner", first, 200, []byte("#EXTM3U\n")); err != nil || status != 409 {
		t.Fatalf("old reply %d %v", status, err)
	}
	started := time.Now()
	r.Close()
	if time.Since(started) > time.Second {
		t.Fatal("shutdown waited on FCUP reply")
	}
}

func TestFCUPFragmentedHLS(t *testing.T) {
	r, address := mediaReceiver(t)
	var requests atomic.Int32
	startFCUPSender(t, address, "fragments", func(q fcupTestRequest) error {
		name := strings.TrimPrefix(q.URL, "mlhls://localhost/")
		switch name {
		case "fragmented.m3u8", "init.mp4", "fragment0.m4s", "fragment1.m4s":
		default:
			return fmt.Errorf("unexpected fMP4 resource")
		}
		data, err := os.ReadFile("../testdata/url/" + name)
		if err != nil {
			return err
		}
		requests.Add(1)
		status, err := fcupAction(address, "fragments", q, 200, data)
		if err != nil {
			return err
		}
		if status != 200 {
			return fmt.Errorf("fragment reply %d", status)
		}
		return nil
	})
	if status, _ := mediaControl(t, address, "POST", "/play", "fragments", []byte("Content-Location: mlhls://localhost/fragmented.m3u8\r\n")); status != 200 {
		t.Fatal(status)
	}
	waitURLState(t, r, "playing")
	f, err := r.ReadFrame()
	if err != nil || f == nil {
		t.Fatalf("fMP4 video %v", err)
	}
	f.Release()
	if status, _ := mediaControl(t, address, "POST", "/rate?value=0", "fragments", nil); status != 200 {
		t.Fatal(status)
	}
	if status, _ := mediaControl(t, address, "POST", "/scrub?position=1.1", "fragments", nil); status != 200 {
		t.Fatal(status)
	}
	if status, _ := mediaControl(t, address, "POST", "/rate?value=1", "fragments", nil); status != 200 {
		t.Fatal(status)
	}
	f, err = r.ReadFrame()
	if err != nil || f == nil {
		t.Fatalf("fMP4 seek video %v", err)
	}
	idr := false
	for _, nal := range bytes.Split(f.Data, []byte{0, 0, 1}) {
		if len(nal) > 0 && nal[0]&31 == 5 {
			idr = true
		}
	}
	f.Release()
	if !idr {
		t.Fatal("fMP4 seek did not start with IDR")
	}
	if status, _ := mediaControl(t, address, "POST", "/stop", "fragments", nil); status != 200 {
		t.Fatal(status)
	}
	if requests.Load() != 4 {
		t.Fatalf("fMP4 cache requests %d", requests.Load())
	}
	r.Close()
}

func TestFCUPControlBodyBudget(t *testing.T) {
	for _, path := range []string{"/action", "/play"} {
		body := strings.Repeat("x", maxControlBody+1)
		wire := fmt.Sprintf("POST %s HTTP/1.1\r\nContent-Length: %d\r\n\r\n%s", path, len(body), body)
		_, err := readRequest(bufio.NewReaderSize(strings.NewReader(wire), maxControlHeaders))
		if (err == nil) != (path == "/action") {
			t.Fatalf("%s budget %v", path, err)
		}
	}
	// Headers stay bounded, even on the larger /action endpoint.
	wire := fmt.Sprintf("POST /action HTTP/1.1\r\nContent-Length: %d\r\n\r\n", maxFCUPActionBody+1)
	if _, err := readRequest(bufio.NewReader(strings.NewReader(wire))); err == nil {
		t.Fatal("accepted oversized FCUP action")
	}
}

func TestFCUPResourceProxy(t *testing.T) {
	origin, _ := url.Parse("mlhls://localhost/key.bin")
	key := playbackKey{peer: "127.0.0.1", session: "proxy"}
	e := &reverseEvents{key: key, requests: make(chan reverseRequest, 1), done: make(chan struct{})}
	r := &Receiver{urlEvents: e}
	s := newFCUPStore(context.Background(), r, key, origin)
	r.urlPlayer = &urlPlayer{key: key, fcup: s}
	location, err := s.open(origin.String())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	var requests atomic.Int32
	done := make(chan error, 1)
	go func() {
		for {
			select {
			case <-s.ctx.Done():
				done <- nil
				return
			case event := <-e.requests:
				var body struct {
					Request fcupTestRequest `plist:"request"`
				}
				if _, err := plist.Unmarshal(event.body, &body); err != nil {
					done <- err
					return
				}
				requests.Add(1)
				data := []byte("0123456789")
				status := uint64(200)
				if strings.HasSuffix(body.Request.URL, "live.m3u8") {
					data = []byte("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\nhttps://cdn.example/video.ts\n")
				}
				if strings.HasSuffix(body.Request.URL, "missing.bin") {
					status = 404
				}
				data, err := plist.Marshal(map[string]any{"type": "unhandledURLResponse", "params": map[string]any{"FCUP_Response_RequestID": body.Request.ID, "FCUP_Response_URL": body.Request.URL, "FCUP_Response_StatusCode": status, "FCUP_Response_Data": data}}, plist.BinaryFormat)
				if err != nil {
					done <- err
					return
				}
				headers := make(textproto.MIMEHeader)
				headers.Set("X-Apple-Session-ID", key.session)
				conn := &idleControlConn{peer: net.TCPAddr{IP: net.ParseIP(key.peer), Port: 1234}}
				q := request{body: data, headers: headers}
				code, _, _, _ := r.handleFCUPAction(q, conn)
				if code != 200 {
					done <- fmt.Errorf("reply status %d", code)
					return
				}
				code, _, _, _ = r.handleFCUPAction(q, conn)
				if code != 409 {
					done <- fmt.Errorf("duplicate status %d", code)
					return
				}
			}
		}
	}()
	t.Cleanup(func() {
		s.cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("proxy sender leaked")
		}
	})
	get := func(method, location, byterange string) (int, []byte) {
		t.Helper()
		q, _ := http.NewRequest(method, location, nil)
		if byterange != "" {
			q.Header.Set("Range", byterange)
		}
		client := http.Client{Timeout: 2 * time.Second}
		res, err := client.Do(q)
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
	if code, data := get("GET", location, "bytes=2-5"); code != 206 || string(data) != "2345" {
		t.Fatalf("range %d %s", code, data)
	}
	if code, data := get("HEAD", location, ""); code != 200 || len(data) != 0 {
		t.Fatalf("HEAD %d %s", code, data)
	}
	if code, data := get("GET", location, ""); code != 200 || string(data) != "0123456789" {
		t.Fatalf("cached GET %d %s", code, data)
	}
	if requests.Load() != 1 {
		t.Fatal("cached resource was fetched again")
	}
	for _, uri := range []string{"file:///tmp/key", "mlhls://other/key", "nfhls://localhost/key"} {
		if code, _ := get("GET", s.base+base64.RawURLEncoding.EncodeToString([]byte(uri)), ""); code != 404 {
			t.Fatal("invalid resource admitted")
		}
	}
	live := s.localURL("mlhls://localhost/live.m3u8")
	if code, _ := get("GET", live, ""); code != 200 {
		t.Fatal(code)
	}
	if code, _ := get("GET", live, ""); code != 200 || requests.Load() != 2 {
		t.Fatal("live cache missing")
	}
	time.Sleep(1100 * time.Millisecond)
	if code, _ := get("GET", live, ""); code != 200 || requests.Load() != 3 {
		t.Fatal("live playlist did not refresh")
	}
	if code, _ := get("GET", s.localURL("mlhls://localhost/missing.bin"), ""); code != 503 {
		t.Fatal("sender failure ignored")
	}
}
