package gpuconv

import "testing"

func TestSurfaceParametersBounds(t *testing.T) {
	s := &Surface{SharedTexture: 1, Width: 129, Height: 131, Rotation: 90}
	p, err := SurfaceParameters(s, 100, 100, true)
	if err != nil || p[0] != 131 || p[1] != 129 || p[3] != 90 || p[4] != 100 || p[5] != 100 || p[8] != 98 || p[9] != 100 || p[10] != 1 {
		t.Fatalf("invalid surface fit: %v %v", p, err)
	}
	for _, size := range [][2]int{{0, 1}, {1, 0}, {8193, 1}, {8192, 8192}} {
		if _, err := SurfaceParameters(s, size[0], size[1], false); err == nil {
			t.Fatalf("accepted output %v", size)
		}
	}
	for _, invalid := range []*Surface{nil, {}, {SharedTexture: 1, Width: 64, Height: 64, Rotation: 45}, {SharedTexture: 1, Width: 8192, Height: 8192}, {SharedTexture: 1, Image: &Image{}, Width: 64, Height: 64}} {
		if err := invalid.Validate(); err == nil {
			t.Fatal("accepted invalid surface")
		}
	}
}
