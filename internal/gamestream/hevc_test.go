package gamestream

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"anykvm/internal/capture"
)

func hevcNALs(types ...byte) []byte {
	var data []byte
	for _, n := range types {
		data = append(data, 0, 0, 0, 1, n<<1, 1, 0xab)
	}
	return data
}
func TestHEVCRecoveryFrames(t *testing.T) {
	for _, kind := range []byte{16, 17, 18, 19, 20, 21} {
		if !isRecoveryFrame(hevcNALs(32, 33, 34, kind), capture.FormatHEVC) {
			t.Fatalf("rejected IRAP %d", kind)
		}
	}
	for _, data := range [][]byte{hevcNALs(19), hevcNALs(32, 33, 19), hevcNALs(32, 33, 34, 1), {0, 0, 1, 0x65, 1}} {
		if isRecoveryFrame(data, capture.FormatHEVC) {
			t.Fatalf("accepted incomplete recovery %x", data)
		}
	}
}
func TestHEVCNegotiation(t *testing.T) {
	base := "a=x-nv-video[0].clientViewportWd:1280\na=x-nv-video[0].clientViewportHt:720\na=x-nv-video[0].maxFPS:60\n"
	for _, tc := range []struct {
		name                string
		support             bool
		format, hdr, chroma int
		ok                  bool
	}{
		{"h264", false, 0, 0, 0, true}, {"hevc", true, 1, 0, 0, true},
		{"unsupported source", false, 1, 0, 0, false}, {"hdr", true, 1, 1, 0, false},
		{"444", true, 1, 0, 1, false}, {"av1", true, 2, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{cfg: Config{EnableHEVC: true, SupportsHEVC: func(index int) bool { return tc.support && index == 2 }}}
			x := &streamSession{server: s, machine: 2, ctx: context.Background()}
			describe := string(x.handleRTSP(rtspRequest{method: "DESCRIBE", headers: make(textproto.MIMEHeader)}))
			if strings.Contains(describe, "sprop-parameter-sets=AAAAAU") != tc.support {
				t.Fatal("incorrect SDP capability")
			}
			err := x.announce([]byte(base + fmt.Sprintf("a=x-nv-vqos[0].bitStreamFormat:%d\na=x-nv-video[0].dynamicRangeMode:%d\na=x-ss-video[0].chromaSamplingType:%d\n", tc.format, tc.hdr, tc.chroma)))
			if (err == nil) != tc.ok {
				t.Fatalf("announce: %v", err)
			}
			if tc.ok && (x.options.Codec == "hevc") != (tc.format == 1) {
				t.Fatal("codec not stored")
			}
		})
	}
}
func TestHEVCHostCapability(t *testing.T) {
	for _, supported := range []bool{false, true} {
		s := &Server{cfg: Config{EnableHEVC: true, Machines: []string{"Test"}, SupportsHEVC: func(int) bool { return supported }}}
		// serverInfo uses the paired state but a plain HTTP request is unpaired.
		s.state.ID = "test"
		w := httptest.NewRecorder()
		s.serverInfo(w, httptest.NewRequest("GET", "http://localhost/serverinfo", nil), false)
		want := "<ServerCodecModeSupport>1</ServerCodecModeSupport>"
		if supported {
			want = "<ServerCodecModeSupport>257</ServerCodecModeSupport>"
		}
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("capability missing: %s", w.Body.String())
		}
	}
}

func TestHEVCDisabledByDefault(t *testing.T) {
	s := &Server{cfg: Config{Machines: []string{"Test"}, SupportsHEVC: func(int) bool { t.Fatal("disabled codec probed hardware"); return true }}}
	x := &streamSession{server: s, ctx: context.Background()}
	w := httptest.NewRecorder()
	s.serverInfo(w, httptest.NewRequest("GET", "http://localhost/serverinfo", nil), false)
	if !strings.Contains(w.Body.String(), "<ServerCodecModeSupport>1</ServerCodecModeSupport>") || !strings.Contains(w.Body.String(), "<MaxLumaPixelsHEVC>0</MaxLumaPixelsHEVC>") {
		t.Fatal("disabled HEVC advertised")
	}
	describe := string(x.handleRTSP(rtspRequest{method: "DESCRIBE", headers: make(textproto.MIMEHeader)}))
	if strings.Contains(describe, "sprop-parameter-sets=AAAAAU") {
		t.Fatal("disabled HEVC in SDP")
	}
	if err := x.announce([]byte("a=x-nv-vqos[0].bitStreamFormat:1\n")); err == nil {
		t.Fatal("disabled HEVC negotiation accepted")
	}
}
