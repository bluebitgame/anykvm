package vnc

import "testing"

func TestTightIsolatesSolidMCUInsideDirtyTile(t *testing.T) {
	pure := rectangle{16, 16, 16, 16}
	rects, fills := prepareTightRegionsSized([]rectangle{{0, 0, 64, 64}}, 16, func(r rectangle) (uint32, bool) {
		return 0x123456, r == pure
	})
	if len(fills) != 1 || fills[pure] != 0x123456 {
		t.Fatalf("solid MCU was swallowed into a JPEG rectangle: %+v", fills)
	}
	coverage := make([]bool, 64*64)
	for _, r := range rects {
		_, fill := fills[r]
		for y := r.y; y < r.y+r.h; y++ {
			for x := r.x; x < r.x+r.w; x++ {
				insidePure := x >= 16 && x < 32 && y >= 16 && y < 32
				if x < 0 || x >= 64 || y < 0 || y >= 64 || coverage[y*64+x] || insidePure != fill {
					t.Fatal("JPEG/Fill overlap, out-of-bounds rectangle, or constant MCU requires JPEG synthesis")
				}
				coverage[y*64+x] = true
			}
		}
	}
	for _, covered := range coverage {
		if !covered {
			t.Fatal("missing pixels in mixed MCU update")
		}
	}
}
