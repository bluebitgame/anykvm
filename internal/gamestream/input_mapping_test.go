//go:build cgo

package gamestream

import (
	"encoding/binary"
	"fmt"
	"math"
	"testing"
)

func TestNativePointerAspectMapping(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		native                 bool
		sw, sh, ow, oh, pw, ph int
		x, y, wantX, wantY     float64
	}{
		{"portrait native center", true, 1328, 2896, 1328, 2896, 2560, 1440, .5, .5, .5, .5},
		{"portrait native left edge", true, 1328, 2896, 1328, 2896, 2560, 1440, (1 - (1328.0/2896)/(2560.0/1440)) / 2, .25, 0, .25},
		{"portrait native right edge", true, 1328, 2896, 1328, 2896, 2560, 1440, (1 + (1328.0/2896)/(2560.0/1440)) / 2, .75, 1, .75},
		{"matching native aspect", true, 1328, 2896, 1328, 2896, 664, 1448, .25, .75, .25, .75},
		{"encoded letterbox unchanged", false, 1328, 2896, 2560, 1440, 2560, 1440, (1 - (1328.0/2896)/(2560.0/1440)) / 2, .25, 0, .25},
		{"ordinary encoder unchanged", false, 2560, 1440, 2560, 1440, 1920, 1080, .25, .75, .25, .75},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &testHID{}
			i := inputState{hid: h, active: true, sourceW: tc.sw, sourceH: tc.sh, outputW: tc.ow, outputH: tc.oh, nativeEncoded: tc.native}
			p := make([]byte, 18)
			binary.BigEndian.PutUint32(p, 14)
			binary.LittleEndian.PutUint32(p[4:], 5)
			for offset, value := range map[int]int{8: int(math.Round(tc.x * float64(tc.pw))), 10: int(math.Round(tc.y * float64(tc.ph))), 14: tc.pw, 16: tc.ph} {
				binary.BigEndian.PutUint16(p[offset:], uint16(value))
			}
			if err := i.handle(p); err != nil {
				t.Fatal(err)
			}
			var x, y int
			if len(h.events) != 1 {
				t.Fatal(h.events)
			}
			if _, err := fmt.Sscanf(h.events[0], "abs:%d,%d", &x, &y); err != nil {
				t.Fatal(err)
			}
			if math.Abs(float64(x)-tc.wantX*32767) > 50 || math.Abs(float64(y)-tc.wantY*32767) > 50 {
				t.Fatalf("got (%d,%d), wanted normalized (%f,%f)", x, y, tc.wantX, tc.wantY)
			}
		})
	}
}
