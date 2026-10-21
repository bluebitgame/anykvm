package gamestream

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
)

func TestOptionalCodecQueriesRespectIndependentSettings(t *testing.T) {
	for _, hevc := range []bool{false, true} {
		for _, av1 := range []bool{false, true} {
			t.Run(fmt.Sprintf("hevc=%v/av1=%v", hevc, av1), func(t *testing.T) {
				hevcCalls, av1Calls := 0, 0
				s := &Server{cfg: Config{
					Machines: []string{"test"}, EnableHEVC: hevc, EnableAV1: av1,
					SupportsHEVC: func(int) bool {
						hevcCalls++
						if !hevc {
							t.Fatal("disabled HEVC queried capability")
						}
						return true
					},
					SupportsAV1: func(int) bool {
						av1Calls++
						if !av1 {
							t.Fatal("disabled AV1 queried capability")
						}
						return true
					},
				}}
				w := httptest.NewRecorder()
				s.serverInfo(w, httptest.NewRequest("GET", "http://localhost/serverinfo", nil), false)
				mask := 1
				if hevc {
					mask |= 0x100
				}
				if av1 {
					mask |= 0x10000
				}
				if !strings.Contains(w.Body.String(), fmt.Sprintf("<ServerCodecModeSupport>%d</ServerCodecModeSupport>", mask)) {
					t.Fatal("incorrect capability advertisement")
				}
				x := &streamSession{server: s, ctx: context.Background()}
				x.handleRTSP(rtspRequest{method: "DESCRIBE", headers: make(textproto.MIMEHeader)})
				beforeHEVC, beforeAV1 := hevcCalls, av1Calls
				for format := 0; format <= 2; format++ {
					x := &streamSession{server: s, ctx: context.Background()}
					body := fmt.Sprintf("a=x-nv-video[0].clientViewportWd:1280\na=x-nv-video[0].clientViewportHt:720\na=x-nv-video[0].maxFPS:30\na=x-nv-vqos[0].bitStreamFormat:%d\n", format)
					err := x.announce([]byte(body))
					want := format == 0 || format == 1 && hevc || format == 2 && av1
					if (err == nil) != want {
						t.Fatalf("format=%d accepted=%v want=%v: %v", format, err == nil, want, err)
					}
					if format == 0 && (hevcCalls != beforeHEVC || av1Calls != beforeAV1) {
						t.Fatal("H.264 announcement queried optional codec capabilities")
					}
				}
				if hevc && hevcCalls == 0 || av1 && av1Calls == 0 {
					t.Fatal("enabled codec declaration was never queried")
				}
			})
		}
	}
}
