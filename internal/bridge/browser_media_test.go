package bridge

import (
	"anykvm/internal/capture"
	"net/http"
	"net/http/httptest"
	"testing"
)

type browserCaptureFixture struct{ capture.Device }

func (*browserCaptureFixture) BrowserMediaState() *capture.BrowserMediaState {
	return &capture.BrowserMediaState{ID: "session"}
}
func (*browserCaptureFixture) BrowserMediaFeedback(capture.BrowserMediaFeedback) bool { return true }
func (*browserCaptureFixture) ServeBrowserMedia(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(206)
}

func TestContainerMediaIsWebOnly(t *testing.T) {
	b := NewBridge([]*MachineSession{{Capture: &browserCaptureFixture{}}}, "alt", 30, 0, 0)
	for _, proto := range []string{"", "gamestream", "vnc", "rdp", "ws"} {
		b.sessionMu.Lock()
		b.activeProto = proto
		b.sessionMu.Unlock()
		state := b.BrowserMediaState()
		accepted := b.BrowserMediaFeedback(capture.BrowserMediaFeedback{})
		res := httptest.NewRecorder()
		b.ServeBrowserMedia(res, httptest.NewRequest("GET", "/airplay/media/token/media", nil))
		if proto == "ws" {
			if state == nil || !accepted || res.Code != 206 {
				t.Fatalf("Web media unavailable %v %v %d", state, accepted, res.Code)
			}
		} else if state != nil || accepted || res.Code != 404 {
			t.Fatalf("%s received container playback", proto)
		}
	}
}
