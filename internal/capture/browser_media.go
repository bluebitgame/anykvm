package capture

import "net/http"

// BrowserMediaState describes a container player independently of pixel capture.
// URLs are relative to the authenticated Web listener, and IDs change on /play.
type BrowserMediaState struct {
	ID            string  `json:"id"`
	URL           string  `json:"url"`
	Version       uint64  `json:"version"`
	SeekVersion   uint64  `json:"seekVersion"`
	Paused        bool    `json:"paused"`
	Position      float64 `json:"position"`
	StartFraction float64 `json:"startFraction"`
}

type MediaTimeRange struct {
	Start    float64 `json:"start"`
	Duration float64 `json:"duration"`
}

// Feedback reports actual browser playback rather than estimated server time.
type BrowserMediaFeedback struct {
	ID       string           `json:"id"`
	Version  uint64           `json:"version"`
	Duration float64          `json:"duration"`
	Position float64          `json:"position"`
	State    string           `json:"state"`
	Ready    bool             `json:"ready"`
	Buffered []MediaTimeRange `json:"buffered"`
	Seekable []MediaTimeRange `json:"seekable"`
}

type BrowserMediaSource interface {
	BrowserMediaState() *BrowserMediaState
	BrowserMediaFeedback(BrowserMediaFeedback) bool
	ServeBrowserMedia(http.ResponseWriter, *http.Request)
}
