package rdp

import (
	"anykvm/internal/gpuconv"
	_ "embed"
	"fmt"
	"runtime"
)

//go:embed shaders/progressive.hlsl
var progressiveShader string

//go:embed shaders/progressive_surface.hlsl
var progressiveSurfaceShader string

type progressiveCompute interface {
	Run([]byte, []uint32) ([]byte, [48]uint32, error)
	Close()
}
type progressiveSurfaceCompute interface {
	PrepareSurface(*gpuconv.Surface, int, int, bool) ([]uint32, bool, error)
	RunSurface([]uint32) ([]byte, [48]uint32, error)
	CommitSurface() error
}

type gpuProgressiveCodec struct {
	compute            progressiveCompute
	quantization       int
	input              []byte
	surfaceActive      bool
	surfaceW, surfaceH int
}

func newGPUProgressive(mode string, quantization int) (nativeProgressiveCodec, error) {
	if mode == "vulkan" && runtime.GOOS != "linux" || mode == "d3d11" && runtime.GOOS != "windows" {
		return nil, fmt.Errorf("Progressive %s backend unavailable on %s", mode, runtime.GOOS)
	}
	compute, err := gpuconv.NewProgressiveCompute(progressiveShader, progressiveSPIRV())
	if err != nil {
		return nil, fmt.Errorf("Progressive GPU: %w", err)
	}
	if err = compute.ConfigureSurface(progressiveSurfaceShader, progressiveSurfaceSPIRV()); err != nil {
		compute.Close()
		return nil, err
	}
	return &gpuProgressiveCodec{compute: compute, quantization: quantization}, nil
}

func (e *progressiveEncoder) prepareSurface(s *gpuconv.Surface, w, h int, force bool) ([]bitmapRect, error) {
	g, ok := e.native.(*gpuProgressiveCodec)
	if !ok {
		return nil, fmt.Errorf("GPU capture requires the Progressive GPU backend")
	}
	compute, ok := g.compute.(progressiveSurfaceCompute)
	if !ok {
		return nil, fmt.Errorf("Progressive backend cannot consume GPU surfaces")
	}
	flags, reset, err := compute.PrepareSurface(s, w, h, force || !g.surfaceActive)
	if err != nil {
		return nil, err
	}
	if reset {
		e.discardRefinements()
	}
	if len(flags) != ((w+63)/64)*((h+63)/64) {
		return nil, fmt.Errorf("invalid GPU dirty tile grid")
	}
	var rects []bitmapRect
	columns := (w + 63) / 64
	for i, flag := range flags {
		if flag > 1 {
			return nil, fmt.Errorf("invalid GPU dirty tile flag")
		}
		if flag != 0 {
			x, y := (i%columns)*64, (i/columns)*64
			rects = append(rects, bitmapRect{x, y, min(64, w-x), min(64, h-y)})
		}
	}
	g.surfaceActive, g.surfaceW, g.surfaceH = true, w, h
	return rects, nil
}
func (e *progressiveEncoder) encodeSurface(w, h int, rects []bitmapRect, frame uint32) ([]byte, error) {
	g, ok := e.native.(*gpuProgressiveCodec)
	if !ok || !g.surfaceActive || g.surfaceW != w || g.surfaceH != h {
		return nil, fmt.Errorf("no prepared Progressive GPU surface")
	}
	return e.encodePrepared(nil, w, h, rects, frame, progressiveBGRA, true)
}
func (e *progressiveEncoder) commitSurface() error {
	g, ok := e.native.(*gpuProgressiveCodec)
	if !ok || !g.surfaceActive {
		return fmt.Errorf("no prepared GPU surface")
	}
	compute, ok := g.compute.(progressiveSurfaceCompute)
	if !ok {
		return fmt.Errorf("GPU surface backend unavailable")
	}
	return compute.CommitSurface()
}
func (*gpuProgressiveCodec) residentCoefficients() {}
func (*gpuProgressiveCodec) name() string {
	if runtime.GOOS == "windows" {
		return "D3D11 compute"
	}
	return "Vulkan compute"
}
func (g *gpuProgressiveCodec) close() { g.compute.Close(); g.input = nil }
func (g *gpuProgressiveCodec) encode(p []byte, w, h int, rects []bitmapRect, frame uint32, first bool, format progressivePixelFormat) ([]byte, error) {
	return g.encodePass(p, w, h, rects, frame, first, format, 0, false, nil)
}

// packProgressiveTiles retains the exact CPU encoder's edge extension. NV12
// stays NV12: the shader reproduces its integer BT.709->RGB->ICT rounding.
func packProgressiveTiles(dst, p []byte, w, h int, tiles []bitmapRect, format progressivePixelFormat) []byte {
	size := 16384
	if format == progressiveNV12 {
		size = 6144
	}
	dst = planarBuffer(dst, len(tiles)*size)
	for i, t := range tiles {
		out := dst[i*size : (i+1)*size]
		if format == progressiveNV12 {
			stride := 2 * ((w + 1) / 2)
			for y := 0; y < 64; y++ {
				for x := 0; x < 64; x++ {
					out[y*64+x] = p[min(h-1, t.y+y)*w+min(w-1, t.x+x)]
				}
			}
			for y := 0; y < 32; y++ {
				for x := 0; x < 32; x++ {
					source := w*h + (min(h-1, t.y+2*y)/2)*stride + (min(w-1, t.x+2*x)/2)*2
					copy(out[4096+y*64+x*2:], p[source:source+2])
				}
			}
		} else {
			for y := 0; y < 64; y++ {
				row := min(h-1, t.y+y) * w
				for x := 0; x < 64; x++ {
					source := (row + min(w-1, t.x+x)) * 4
					copy(out[(y*64+x)*4:], p[source:source+4])
				}
			}
		}
	}
	return dst
}
func (g *gpuProgressiveCodec) encodePass(p []byte, w, h int, rects []bitmapRect, frame uint32, first bool, format progressivePixelFormat, bits int, upgrade bool, _ []int32) ([]byte, error) {
	var tiles []bitmapRect
	for _, r := range rects {
		for y := r.y; y < r.y+r.h; y += 64 {
			for x := r.x; x < r.x+r.w; x += 64 {
				tiles = append(tiles, bitmapRect{x, y, min(64, w-x), min(64, h-y)})
			}
		}
	}
	if len(tiles) < 1 || len(tiles) > 8 {
		return nil, fmt.Errorf("invalid Progressive GPU tile batch")
	}
	meta := make([]uint32, 16)
	meta[0], meta[1], meta[2], meta[3] = uint32(len(tiles)), uint32(format), uint32(g.quantization), uint32(bits)
	meta[5] = 8
	if bits != 0 {
		meta[4] = 1
		meta[5] = uint32(((w + 63) / 64) * ((h + 63) / 64))
	}
	if upgrade {
		meta[4] = 2
	}
	resident := !upgrade && g.surfaceActive && p == nil
	if resident {
		meta[6], meta[7] = uint32(w*4), uint32(h)
	}
	for i, t := range tiles {
		meta[8+i] = uint32(i)
		if bits != 0 || resident {
			meta[8+i] = uint32(progressiveTileIndex(w, t.x, t.y))
		}
	}
	input := []byte{0, 0, 0, 0}
	if !upgrade && !resident {
		g.input = packProgressiveTiles(g.input, p, w, h, tiles, format)
		input = g.input
	}
	var packed []byte
	var lengths [48]uint32
	var err error
	if resident {
		packed, lengths, err = g.compute.(progressiveSurfaceCompute).RunSurface(meta)
	} else {
		packed, lengths, err = g.compute.Run(input, meta)
	}
	if err != nil {
		return nil, err
	}
	streams, err := progressiveGPUStreams(packed, lengths, len(tiles), upgrade)
	if err != nil {
		return nil, err
	}
	var encoded []byte
	for i, t := range tiles {
		c := streams[i*6 : (i+1)*6]
		body := join([]byte{0, 0, 0}, u16(t.x/64), u16(t.y/64))
		kind := 0xccc5
		if upgrade {
			kind = 0xccc7
			body = append(body, 255)
			for _, s := range c {
				body = append(body, u16(len(s))...)
			}
			for _, s := range c {
				body = append(body, s...)
			}
		} else {
			body = append(body, 0)
			if bits != 0 {
				kind = 0xccc6
				body = append(body, 0)
			}
			for _, j := range []int{0, 2, 4} {
				body = append(body, u16(len(c[j]))...)
			}
			body = append(body, 0, 0)
			for _, j := range []int{0, 2, 4} {
				body = append(body, c[j]...)
			}
		}
		encoded = append(encoded, progressiveBlock(kind, body)...)
	}
	if bits != 0 {
		return progressiveLayeredWire(rects, frame, first, g.quantization, bits, upgrade, encoded, len(tiles)), nil
	}
	return progressiveSimpleWire(rects, frame, first, g.quantization, encoded, len(tiles)), nil
}

// Validate every length before slicing GPU readback. A driver/shader failure
// cannot turn malformed lengths or padding into a partially sent RDP update.
func progressiveGPUStreams(packed []byte, lengths [48]uint32, tiles int, upgrade bool) ([][]byte, error) {
	if tiles < 1 || tiles > 8 {
		return nil, fmt.Errorf("invalid Progressive GPU stream count")
	}
	out := make([][]byte, tiles*6)
	offset := 0
	for i := range out {
		n := int(lengths[i])
		padded := (n + 3) &^ 3
		if n > 32768 || padded > len(packed)-offset || (!upgrade && i%2 == 1 && n != 0) || (i%2 == 0 && n == 0) {
			return nil, fmt.Errorf("invalid Progressive GPU segment length")
		}
		out[i] = packed[offset : offset+n]
		offset += padded
	}
	if offset != len(packed) {
		return nil, fmt.Errorf("unexpected Progressive GPU trailing data")
	}
	return out, nil
}
