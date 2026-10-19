package wsmedia

import "testing"

func TestH264PolicyAndUpdate(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, supported := range []bool{false, true} {
			var got bool
			called := false
			s := NewServer(Config{DisableH264: !enabled, OnVideoCodecsChanged: func(h264, hevc, av1 bool) { got = h264; called = true }})
			body := []byte(`{"h264":false}`)
			if supported {
				body = []byte(`{"h264":true}`)
			}
			s.handleHello(body)
			if !called || got != (enabled && supported) {
				t.Fatalf("enabled=%v supported=%v got=%v", enabled, supported, got)
			}
			s.UpdateVideoCodecs(!enabled, false, false)
			s.handleHello(body)
			if got != (!enabled && supported) {
				t.Fatal("H.264 policy update ignored")
			}
		}
	}
}
