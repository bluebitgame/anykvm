package wsmedia

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
)

func TestAV1BrowserOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, client := range []bool{false, true} {
			called := false
			s := NewServer(Config{EnableAV1: enabled, OnVideoCodecsChanged: func(h264, hevc, av1 bool) {
				called = true
				if !h264 || hevc || av1 != (enabled && client) {
					t.Fatalf("caps %v %v %v", h264, hevc, av1)
				}
			}})
			body := []byte(`{"h264":true,"av1":false}`)
			if client {
				body = []byte(`{"h264":true,"av1":true}`)
			}
			s.handleHello(body)
			if !called {
				t.Fatal("missing capability callback")
			}
		}
	}
}

func TestHEVCBrowserOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, client := range []bool{false, true} {
			called := false
			s := NewServer(Config{DisableHEVC: !enabled, OnVideoCodecsChanged: func(h264, hevc, av1 bool) {
				called = true
				if !h264 || hevc != (enabled && client) || av1 {
					t.Fatalf("caps %v %v %v", h264, hevc, av1)
				}
			}})
			body := []byte(`{"h264":true,"hevc":false}`)
			if client {
				body = []byte(`{"h264":true,"hevc":true}`)
			}
			s.handleHello(body)
			if !called {
				t.Fatal("missing capability callback")
			}
		}
	}
}
func TestAV1WebMessagePreservesOBUs(t *testing.T) {
	data, err := os.ReadFile("../testdata/stream.ivf")
	if err != nil {
		t.Fatal(err)
	}
	data = data[32:]
	for frame := 0; frame < 6; frame++ {
		n := int(binary.LittleEndian.Uint32(data))
		au := data[12 : 12+n]
		data = data[12+n:]
		msg, err := encodeAV1Frame(au)
		if err != nil {
			t.Fatal(err)
		}
		if msg[0] != msgAV1 || (msg[1] == 1) != (frame%3 == 0) || !bytes.Equal(msg[3+int(msg[2]):], au) {
			t.Fatal("AV1 framing corrupted")
		}
		if msg[1] == 1 && string(msg[3:3+int(msg[2])]) != "av01.0.00M.08" {
			t.Fatal("codec missing")
		}
	}
	if _, err := encodeAV1Frame([]byte{0x32, 4, 0}); err == nil {
		t.Fatal("truncated OBU accepted")
	}
}
