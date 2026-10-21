package jpegcodec

import (
	"image"
	"math/rand"
	"reflect"
	"testing"
)

func TestDesktopDamageCandidates(t *testing.T) {
	state := desktopDamageState{}
	d := &DesktopDamage{Stream: 5, Previous: 0, Sequence: 1, SourceWidth: 193, SourceHeight: 131, Valid: true}
	if _, valid := state.candidateTiles(d, 193, 131, 0); valid {
		t.Fatal("first frame filtered")
	}
	state.commit(d)
	d.Previous, d.Sequence = 1, 2
	d.Rects = []image.Rectangle{image.Rect(70, 70, 72, 72), image.Rect(192, 130, 193, 131), image.Rect(70, 70, 72, 72)}
	got, valid := state.candidateTiles(d, 193, 131, 0)
	if !valid || !reflect.DeepEqual(got, []uint32{5, 11}) {
		t.Fatalf("candidate tiles=%v valid=%v", got, valid)
	}
	state.commit(d)
	// Repeated frames can explicitly report no updates.
	d.Previous = d.Sequence
	d.Rects = nil
	if got, valid := state.candidateTiles(d, 193, 131, 0); !valid || len(got) != 0 {
		t.Fatal("empty update did not skip comparison")
	}
	// A skipped acquisition, new capture session, invalid metadata or rectangle
	// must never filter the comparison, even when output geometry is unchanged.
	for _, mutate := range []func(*DesktopDamage){
		func(d *DesktopDamage) { d.Previous++ }, func(d *DesktopDamage) { d.Stream++ },
		func(d *DesktopDamage) { d.Valid = false }, func(d *DesktopDamage) { d.Sequence = 0 },
		func(d *DesktopDamage) { d.Rects = []image.Rectangle{image.Rect(-1, 0, 2, 2)} },
		func(d *DesktopDamage) { d.Rects = []image.Rectangle{image.Rect(0, 0, 194, 131)} },
	} {
		bad := *d
		mutate(&bad)
		if _, valid := state.candidateTiles(&bad, 193, 131, 0); valid {
			t.Fatalf("unsafe metadata accepted: %+v", bad)
		}
	}
	bad := *d
	bad.Valid = false
	bad.Previous = 2
	bad.Sequence = 3
	state.commit(&bad) // Full comparison consumed the frame despite bad metadata.
	next := *d
	next.Previous = 3
	next.Sequence = 4
	if _, valid := state.candidateTiles(&next, 193, 131, 0); !valid {
		t.Fatal("valid metadata did not recover after full comparison")
	}
	state.commit(nil)
	if _, valid := state.candidateTiles(&next, 193, 131, 0); valid {
		t.Fatal("missing acquisition identity reused old baseline")
	}
}

// Brute-force the JPEG sampler against rotated/scaled update rectangles. Every
// output pixel that samples a dirty source pixel must have a candidate tile.
func TestDesktopDamageSamplerCoverage(t *testing.T) {
	rng := rand.New(rand.NewSource(184))
	for _, rotation := range []int{0, 90, 180, 270} {
		for _, output := range []image.Point{{1, 1}, {65, 17}, {193, 131}, {401, 257}} {
			const sw, sh = 193, 131
			dw, dh := sw, sh
			if rotation == 90 || rotation == 270 {
				dw, dh = sh, sw
			}
			for trial := 0; trial < 25; trial++ {
				x, y := rng.Intn(sw), rng.Intn(sh)
				r := image.Rect(x, y, min(sw, x+1+rng.Intn(35)), min(sh, y+1+rng.Intn(35)))
				d := &DesktopDamage{Stream: 7, Previous: 1, Sequence: 2, SourceWidth: sw, SourceHeight: sh, Valid: true, Rects: []image.Rectangle{r}}
				state := desktopDamageState{stream: 7, sequence: 1, valid: true}
				tiles, valid := state.candidateTiles(d, output.X, output.Y, rotation)
				if !valid {
					t.Fatal("valid geometry rejected")
				}
				selected := map[uint32]bool{}
				for _, tile := range tiles {
					if selected[tile] {
						t.Fatal("duplicate tile")
					}
					selected[tile] = true
				}
				for oy := 0; oy < output.Y; oy++ {
					for ox := 0; ox < output.X; ox++ {
						// Match float32 multiply/divide as well as ordinary exact arithmetic.
						for _, single := range []bool{false, true} {
							dx, dy := int((float64(ox)+.5)*float64(dw)/float64(output.X)), int((float64(oy)+.5)*float64(dh)/float64(output.Y))
							if single {
								dx = int((float32(ox) + .5) * float32(dw) / float32(output.X))
								dy = int((float32(oy) + .5) * float32(dh) / float32(output.Y))
							}
							sx, sy := dx, dy
							switch rotation {
							case 90:
								sx, sy = dy, dw-1-dx
							case 180:
								sx, sy = dw-1-dx, dh-1-dy
							case 270:
								sx, sy = dh-1-dy, dx
							}
							if image.Pt(sx, sy).In(r) && !selected[uint32((oy/64)*((output.X+63)/64)+ox/64)] {
								t.Fatalf("missed update rotation=%d output=%v rect=%v pixel=(%d,%d)", rotation, output, r, ox, oy)
							}
						}
					}
				}
			}
		}
	}
}
