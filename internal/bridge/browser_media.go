package bridge

import (
	"anykvm/internal/capture"
	"net/http"
)

// Container playback belongs only to the active Web consumer. Other protocols
// continue to consume the mirror's pixel/encoded-frame capture interface.
func (b *Bridge) browserMediaSource() capture.BrowserMediaSource {
	b.sessionMu.Lock()
	web := b.activeProto == "ws"
	b.sessionMu.Unlock()
	if !web {
		return nil
	}
	dev := b.activeDevice()
	source, _ := dev.(capture.BrowserMediaSource)
	return source
}

func (b *Bridge) BrowserMediaState() *capture.BrowserMediaState {
	if source := b.browserMediaSource(); source != nil {
		return source.BrowserMediaState()
	}
	return nil
}
func (b *Bridge) BrowserMediaFeedback(f capture.BrowserMediaFeedback) bool {
	if source := b.browserMediaSource(); source != nil {
		return source.BrowserMediaFeedback(f)
	}
	return false
}
func (b *Bridge) ServeBrowserMedia(w http.ResponseWriter, q *http.Request) {
	if source := b.browserMediaSource(); source != nil {
		source.ServeBrowserMedia(w, q)
		return
	}
	http.NotFound(w, q)
}
