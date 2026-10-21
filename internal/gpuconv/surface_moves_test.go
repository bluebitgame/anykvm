package gpuconv

import "testing"

func TestSurfaceMoveParameters(t *testing.T) {
	s := &Surface{SharedTexture: 1, Width: 192, Height: 128}
	move := SurfaceMove{SourceX: 3, SourceY: 4, DestinationX: 64, DestinationY: 64, Width: 64, Height: 64}
	p, err := SurfaceMoveParameters(s, 192, 128, false, []SurfaceMove{move})
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 24 || p[11] != 1 || p[20] != 3 || p[21] != 4 || p[12] != ^uint32(0) {
		t.Fatalf("unexpected ABI: %v", p)
	}
	for _, bad := range []SurfaceMove{{SourceX: -1, Width: 64, Height: 64}, {SourceX: 150, Width: 64, Height: 64}, {DestinationX: 1, Width: 64, Height: 64}, {DestinationY: 128, Width: 64, Height: 64}, {Width: 32, Height: 64}} {
		if _, err := SurfaceMoveParameters(s, 192, 128, false, []SurfaceMove{bad}); err == nil {
			t.Fatalf("accepted invalid move: %+v", bad)
		}
	}
	if _, err := SurfaceMoveParameters(s, 192, 128, false, []SurfaceMove{move, move}); err == nil {
		t.Fatal("duplicate destination accepted")
	}
}
