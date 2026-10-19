package gamestream

import (
	"anykvm/internal/capture"
	"context"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
)

func TestAV1CapabilityAndNegotiation(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, hardware := range []bool{false, true} {
			calls := 0
			s := &Server{cfg: Config{EnableAV1: enabled, Machines: []string{"test"}, SupportsAV1: func(i int) bool { calls++; return i == 0 && hardware }}}
			w := httptest.NewRecorder()
			s.serverInfo(w, httptest.NewRequest("GET", "http://localhost/serverinfo", nil), false)
			want := "<ServerCodecModeSupport>1</ServerCodecModeSupport>"
			if enabled && hardware {
				want = "<ServerCodecModeSupport>65537</ServerCodecModeSupport>"
			}
			if !strings.Contains(w.Body.String(), want) {
				t.Fatal(w.Body.String())
			}
			x := &streamSession{server: s, ctx: context.Background()}
			describe := string(x.handleRTSP(rtspRequest{method: "DESCRIBE", headers: make(textproto.MIMEHeader)}))
			if strings.Contains(describe, "AV1/90000") != (enabled && hardware) {
				t.Fatal(describe)
			}
			base := "a=x-nv-video[0].clientViewportWd:1280\na=x-nv-video[0].clientViewportHt:720\na=x-nv-video[0].maxFPS:30\na=x-nv-vqos[0].bitStreamFormat:2\n"
			err := x.announce([]byte(base))
			if (err == nil) != (enabled && hardware) {
				t.Fatalf("enabled=%v hardware=%v: %v", enabled, hardware, err)
			}
			if err == nil && (x.options.Codec != "av1" || x.options.videoFormat() != capture.FormatAV1) {
				t.Fatal(x.options)
			}
			for _, extra := range []string{"a=x-nv-video[0].dynamicRangeMode:1\n", "a=x-ss-video[0].chromaSamplingType:1\n"} {
				if x.announce([]byte(base+extra)) == nil {
					t.Fatal("accepted HDR/444")
				}
			}
			if !enabled && calls != 0 {
				t.Fatal("disabled AV1 probed hardware")
			}
		}
	}
}
