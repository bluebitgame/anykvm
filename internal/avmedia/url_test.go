package avmedia

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func urlRuntime(t *testing.T) {
	t.Helper()
	if os.Getenv("ANYKVM_TEST_AVMEDIA") == "" {
		t.Skip("set ANYKVM_TEST_AVMEDIA and ANYKVM_FFMPEG_LIBDIR")
	}
	if err := URLMediaAvailable(); err != nil {
		t.Fatal(err)
	}
}
func TestURLMediaMP4AndHLS(t *testing.T) {
	urlRuntime(t)
	server := httptest.NewServer(http.FileServer(http.Dir("../testdata/url")))
	defer server.Close()
	for _, name := range []string{"movie.mp4", "movie.m3u8", "master.m3u8", "fragmented.m3u8"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			source, err := OpenMedia(ctx, server.URL+"/"+name)
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			if source.Duration() < 1.9 || source.Duration() > 2.5 {
				t.Fatalf("duration %v", source.Duration())
			}
			video, audio := 0, 0
			var lastDTS int64 = -1000000
			var hasReorder bool
			for {
				f, err := source.Read()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if f == nil {
					continue
				}
				if f.Audio {
					audio++
					if len(f.Config) < 2 || f.Config[0]>>3 != 2 || f.SampleRate != 44100 || f.Channels != 2 || len(f.Data) < 2 || f.Data[0] == 0xff && f.Data[1]&0xf0 == 0xf0 {
						t.Fatalf("invalid AAC: %+v", f)
					}
				} else {
					video++
					if f.Width != 64 || f.Height != 48 || f.Codec != "h264" || len(f.Data) < 5 || f.Data[0] != 0 || f.Data[1] != 0 || f.Data[2] != 0 || f.Data[3] != 1 {
						t.Fatalf("invalid H264: %+v", f)
					}
					if f.DecodeTimestamp < lastDTS {
						t.Fatal("decode timestamps moved backwards")
					}
					lastDTS = f.DecodeTimestamp
					if f.Timestamp != f.DecodeTimestamp {
						hasReorder = true
					}
				}
				f.Release()
			}
			if video != 20 || audio < 85 || !hasReorder {
				t.Fatalf("video=%d audio=%d reorder=%t", video, audio, hasReorder)
			}
			// Every seek must begin with a decodable IDR, including after
			// EOF and while a previous fMP4 preroll queue is still nonempty.
			for _, position := range []float64{1.1, 0.1, 1.1} {
				if err := source.Seek(position); err != nil {
					t.Fatal(err)
				}
				found := false
				for n := 0; n < 20; n++ {
					f, err := source.Read()
					if err != nil || f == nil {
						t.Fatalf("seek %.1f: %v", position, err)
					}
					if !f.Audio {
						// Annex-B permits both three- and four-byte start codes.
						for i := 0; i+3 < len(f.Data); i++ {
							if bytes.Equal(f.Data[i:i+3], []byte{0, 0, 1}) && f.Data[i+3]&31 == 5 {
								found = true
								break
							}
						}
						f.Release()
						break
					}
					f.Release()
				}
				if !found {
					t.Fatalf("seek %.1f did not start at an IDR", position)
				}
			}

		})
	}
}
func TestURLMediaCancelOpen(t *testing.T) {
	urlRuntime(t)
	entered := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { entered <- struct{}{}; <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		s, err := OpenMedia(ctx, server.URL+"/blocked.mp4")
		if s != nil {
			s.Close()
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("URL request not started")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled open succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not interrupt URL open")
	}
}
func TestURLMediaRejectLocalProtocols(t *testing.T) {
	for _, raw := range []string{"file:///etc/passwd", "pipe:0", "ftp://example.com/a", "http://u:p@example.com/a", "http://", "http://example.com/a\n"} {
		if _, err := OpenMedia(context.Background(), raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}
func TestURLMediaHLSRejectLocalSegment(t *testing.T) {
	urlRuntime(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2,\nfile:///etc/passwd\n#EXT-X-ENDLIST\n")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	source, err := OpenMedia(ctx, server.URL+"/bad.m3u8")
	if source != nil {
		source.Close()
	}
	if err == nil {
		t.Fatal("opened local HLS segment")
	}
	if !strings.Contains(err.Error(), "media") {
		t.Fatal(err)
	}
}

func TestURLMediaHTTPSCertificateAndRedirect(t *testing.T) {
	urlRuntime(t)
	files := http.FileServer(http.Dir("../testdata/url"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/movie.mp4", http.StatusFound)
			return
		}
		files.ServeHTTP(w, r)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	source, err := OpenMedia(ctx, server.URL+"/redirect")
	if err != nil {
		t.Fatal(err)
	}
	source.Close()
	untrusted := httptest.NewTLSServer(files)
	defer untrusted.Close()
	source, err = OpenMedia(ctx, untrusted.URL+"/movie.mp4")
	if source != nil {
		source.Close()
	}
	if err == nil {
		t.Fatal("accepted untrusted HTTPS certificate")
	}
}

func TestURLMediaCloseInterruptsRead(t *testing.T) {
	urlRuntime(t)
	var block atomic.Bool
	entered := make(chan struct{}, 1)
	files := http.FileServer(http.Dir("../testdata/url"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if block.Load() && strings.HasSuffix(r.URL.Path, ".ts") {
			entered <- struct{}{}
			<-r.Context().Done()
			return
		}
		files.ServeHTTP(w, r)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	source, err := OpenMedia(ctx, server.URL+"/movie.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	for {
		packet, err := source.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if packet != nil {
			packet.Release()
		}
	}
	block.Store(true)
	if err := source.Seek(1.1); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		packet, err := source.Read()
		if packet != nil {
			packet.Release()
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("HLS read did not request segment after seek")
	}
	closed := make(chan struct{})
	go func() { source.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not interrupt HLS read")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("blocked HLS read leaked")
	}
}
