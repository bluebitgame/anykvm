package wsmedia

import (
	"anykvm/internal/capture"
	"encoding/json"
	"io"
	"net/http"
)

func (s *Server) handleBrowserMediaState(w http.ResponseWriter, q *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.cfg.BrowserMedia == nil {
		http.NotFound(w, q)
		return
	}
	switch q.Method {
	case "GET":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.cfg.BrowserMedia.BrowserMediaState())
	case "POST":
		// Container feedback is same-origin. Do not allow forms from unrelated
		// pages to drive the sender's playback state using ambient Basic auth.
		if q.Header.Get("Content-Type") != "application/json" || q.Header.Get("Sec-Fetch-Site") == "cross-site" {
			w.WriteHeader(403)
			return
		}
		q.Body = http.MaxBytesReader(w, q.Body, 4096)
		decoder := json.NewDecoder(q.Body)
		decoder.DisallowUnknownFields()
		var feedback capture.BrowserMediaFeedback
		if decoder.Decode(&feedback) != nil || decoder.Decode(new(any)) != io.EOF {
			w.WriteHeader(400)
			return
		}
		if !s.cfg.BrowserMedia.BrowserMediaFeedback(feedback) {
			w.WriteHeader(409)
			return
		}
		w.WriteHeader(204)
	default:
		w.WriteHeader(405)
	}
}
