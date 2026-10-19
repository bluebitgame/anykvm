package wsmedia

import (
	"anykvm/internal/capture"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

type browserSourceFixture struct {
	feedback capture.BrowserMediaFeedback
	accepted bool
}

func (*browserSourceFixture) BrowserMediaState() *capture.BrowserMediaState {
	return &capture.BrowserMediaState{ID: "session", URL: "/airplay/media/session/media"}
}
func (f *browserSourceFixture) BrowserMediaFeedback(p capture.BrowserMediaFeedback) bool {
	f.feedback = p
	return f.accepted
}
func (*browserSourceFixture) ServeBrowserMedia(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(206)
	_, _ = w.Write([]byte("segment"))
}

func TestBrowserMediaUsesWebAuthentication(t *testing.T) {
	source := &browserSourceFixture{accepted: true}
	s := NewServer(Config{Password: "secret", BrowserMedia: source})
	handler, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	call := func(method, path, body string, auth bool, site string) int {
		t.Helper()
		q, _ := http.NewRequest(method, server.URL+path, bytes.NewBufferString(body))
		q.Header.Set("Content-Type", "application/json")
		q.Header.Set("Sec-Fetch-Site", site)
		if auth {
			q.SetBasicAuth("", "secret")
		}
		res, err := http.DefaultClient.Do(q)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res.StatusCode
	}
	for _, path := range []string{"/airplay/media-state", "/airplay/media/session/media"} {
		if code := call("GET", path, "", false, ""); code != 401 {
			t.Fatal("bypassed authentication", code)
		}
	}
	if code := call("GET", "/airplay/media-state", "", true, ""); code != 200 {
		t.Fatal(code)
	}
	if code := call("GET", "/airplay/media/session/media", "", true, ""); code != 206 {
		t.Fatal(code)
	}
	if code := call("POST", "/airplay/media-state", `{"id":"session","version":1,"position":1.5,"state":"playing"}`, true, "same-origin"); code != 204 || source.feedback.Position != 1.5 {
		t.Fatal(code, source.feedback)
	}
	if code := call("POST", "/airplay/media-state", `{"id":"session"}`, true, "cross-site"); code != 403 {
		t.Fatal(code)
	}
	if code := call("POST", "/airplay/media-state", `{"id":"session"} {}`, true, ""); code != 400 {
		t.Fatal(code)
	}
	source.accepted = false
	if code := call("POST", "/airplay/media-state", `{"id":"old"}`, true, ""); code != 409 {
		t.Fatal(code)
	}
}
