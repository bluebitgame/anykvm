package gpuconv

import "fmt"

// SurfaceMove maps one complete destination tile to pixels in the last committed
// fitted picture. Sources may be unaligned. Only metadata crosses the CPU.
type SurfaceMove struct{ SourceX, SourceY, DestinationX, DestinationY, Width, Height int }

// SurfaceMoveParameters extends the 12-word surface ABI with source XY per tile.
// A sentinel selects ordinary comparison at tiles without a proposed copy.
func SurfaceMoveParameters(s *Surface, w, h int, force bool, moves []SurfaceMove) ([]uint32, error) {
	base, err := SurfaceParameters(s, w, h, force)
	if err != nil {
		return nil, err
	}
	p := append([]uint32(nil), base[:]...)
	if len(moves) == 0 {
		return p, nil
	}
	cols, rows := (w+63)/64, (h+63)/64
	p[11] = 1
	for i := 0; i < cols*rows*2; i++ {
		p = append(p, ^uint32(0))
	}
	for _, m := range moves {
		if m.Width != 64 || m.Height != 64 || m.DestinationX < 0 || m.DestinationY < 0 || m.DestinationX%64 != 0 || m.DestinationY%64 != 0 || m.DestinationX > w-64 || m.DestinationY > h-64 || m.SourceX < 0 || m.SourceY < 0 || m.SourceX > w-64 || m.SourceY > h-64 {
			return nil, fmt.Errorf("invalid surface copy candidate")
		}
		i := 12 + 2*((m.DestinationY/64)*cols+m.DestinationX/64)
		if p[i] != ^uint32(0) {
			return nil, fmt.Errorf("duplicate surface copy destination")
		}
		p[i], p[i+1] = uint32(m.SourceX), uint32(m.SourceY)
	}
	return p, nil
}
