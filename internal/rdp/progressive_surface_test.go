package rdp

import (
	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"anykvm/internal/framebuf"
	"anykvm/internal/gpuconv"
	"bytes"
	"encoding/binary"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// This fixture executes both shipped HLSL programs, retaining the prepared and
// committed frames separately just like the GPU runtimes. Pixel access exists
// only in the test process, never in the production Surface descriptor.
type surfaceComputeFixture struct {
	program                    *shaderCompute
	frames                     map[*gpuconv.Surface][]byte
	current, previous          []byte
	w, h                       int
	prepared                   chan struct{}
	committed                  chan struct{}
	uploads, resident, commits atomic.Int32
}

func (f *surfaceComputeFixture) Run(p []byte, m []uint32) ([]byte, [48]uint32, error) {
	if m[4] != 2 {
		f.uploads.Add(1)
	}
	return f.program.Run(p, m)
}
func (f *surfaceComputeFixture) Close() { f.program.Close() }
func (f *surfaceComputeFixture) PrepareSurface(s *gpuconv.Surface, w, h int, force bool) ([]uint32, bool, error) {
	if f.w != w || f.h != h || f.previous == nil {
		force = true
		f.previous = make([]byte, w*h*4)
	}
	f.w, f.h = w, h
	current, flags, err := executeSurfaceShader(f.program, s, w, h, force, f.frames[s], f.previous)
	if err != nil {
		return nil, false, err
	}
	f.current = current
	if f.prepared != nil {
		f.prepared <- struct{}{}
	}
	return flags, false, nil
}
func (f *surfaceComputeFixture) RunSurface(m []uint32) ([]byte, [48]uint32, error) {
	f.resident.Add(1)
	return f.program.Run(f.current, m)
}
func (f *surfaceComputeFixture) CommitSurface() error {
	f.previous = append(f.previous[:0], f.current...)
	f.commits.Add(1)
	if f.committed != nil {
		f.committed <- struct{}{}
	}
	return nil
}
func executeSurfaceShader(program *shaderCompute, s *gpuconv.Surface, w, h int, force bool, raw, previous []byte) ([]byte, []uint32, error) {
	p, err := gpuconv.SurfaceParameters(s, w, h, force)
	if err != nil {
		return nil, nil, err
	}
	if len(raw)%int(p[1]) != 0 {
		return nil, nil, fmt.Errorf("invalid surface test row stride")
	}
	p[2] = uint32(len(raw) / int(p[1]))
	if p[2] < p[0]*4 || p[2]%4 != 0 {
		return nil, nil, fmt.Errorf("invalid surface test pitch")
	}
	if len(raw) != int(p[2]*p[1]) || len(previous) != w*h*4 {
		return nil, nil, fmt.Errorf("invalid surface test input")
	}
	m := make([]uint32, 16)
	copy(m, p[:])
	m[15] = 0x53555246
	input := append(append([]byte(nil), raw...), previous...)
	packed, lengths, err := program.Run(input, m)
	if err != nil {
		return nil, nil, err
	}
	if int(lengths[0]) != w*h*4 || len(packed) != w*h*4+int(lengths[1])*4 {
		return nil, nil, fmt.Errorf("invalid surface fixture output")
	}
	flags := make([]uint32, int(lengths[1]))
	for i := range flags {
		flags[i] = binary.LittleEndian.Uint32(packed[w*h*4+i*4:])
	}
	return packed[:w*h*4], flags, nil
}

func surfaceCPUReference(raw []byte, s *gpuconv.Surface, w, h int) []byte {
	sw, sh := s.Width, s.Height
	rawW, rawH := sw, sh
	if s.Rotation == 90 || s.Rotation == 270 {
		rawW, rawH = sh, sw
	}
	pixels := make([]byte, sw*sh*4)
	for y := 0; y < sh; y++ {
		for x := 0; x < sw; x++ {
			sx, sy := x, y
			switch s.Rotation {
			case 90:
				sx, sy = y, rawH-1-x
			case 180:
				sx, sy = rawW-1-x, rawH-1-y
			case 270:
				sx, sy = rawW-1-y, x
			}
			copy(pixels[(y*sw+x)*4:], raw[(sy*rawW+sx)*4:(sy*rawW+sx)*4+3])
			pixels[(y*sw+x)*4+3] = 255
		}
	}
	out, _ := scaleBitmap(pixels, bridge.Frame{Width: sw, Height: sh}, w, h)
	return out
}
func TestProgressiveSurfaceShader(t *testing.T) {
	executable := shaderExecutable(t)
	program := startShader(t, executable).(*shaderCompute)
	defer program.Close()
	for _, size := range [][2]int{{1, 1}, {3, 2}, {65, 67}, {129, 131}} {
		for _, rotation := range []int{0, 90, 180, 270} {
			sw, sh := size[0], size[1]
			s := &gpuconv.Surface{SharedTexture: 1, Width: sw, Height: sh, Rotation: rotation}
			if rotation == 90 || rotation == 270 {
				s.Width, s.Height = sh, sw
			}
			raw := progressivePixels(sw, sh, "gradient")
			for _, output := range [][2]int{{s.Width, s.Height}, {93, 47}} {
				w, h := output[0], output[1]
				previous := make([]byte, w*h*4)
				for _, phase := range []string{"initial", "unchanged", "alpha", "changed", "refresh"} {
					if phase == "alpha" {
						raw[3] ^= 255
					}
					if phase == "changed" {
						raw[len(raw)-4] ^= 128
					}
					force := phase == "initial" || phase == "refresh"
					got, flags, err := executeSurfaceShader(program, s, w, h, force, raw, previous)
					if err != nil {
						t.Fatal(err)
					}
					want := surfaceCPUReference(raw, s, w, h)
					if !bytes.Equal(got, want) {
						t.Fatalf("GPU resample mismatch source=%v rotation=%d output=%v phase=%s", size, rotation, output, phase)
					}
					old := previous
					if force {
						old = nil
					}
					wantRects := dirtyBitmapRects(want, old, w, h)
					var gotRects []bitmapRect
					for i, flag := range flags {
						if flag > 1 {
							t.Fatal("invalid dirty flag")
						}
						if flag != 0 {
							x, y := (i%((w+63)/64))*64, (i/((w+63)/64))*64
							gotRects = append(gotRects, bitmapRect{x, y, min(64, w-x), min(64, h-y)})
						}
					}
					if !reflect.DeepEqual(gotRects, wantRects) {
						t.Fatalf("GPU dirty mismatch phase=%s: got %v want %v", phase, gotRects, wantRects)
					}
					if phase != "changed" {
						previous = append(previous[:0], got...)
					} // Check uncommitted changes against the old reference on refresh.
				}
			}
		}
	}
	// Vulkan conversion buffers have aligned row pitches rather than tightly
	// packed pixels. Padding must never enter either sampling or dirty decisions.
	s := &gpuconv.Surface{SharedTexture: 1, Width: 65, Height: 67}
	raw := progressivePixels(65, 67, "noise")
	const pitch = 512
	padded := bytes.Repeat([]byte{0xa5}, pitch*67)
	for y := 0; y < 67; y++ {
		copy(padded[y*pitch:], raw[y*65*4:(y+1)*65*4])
	}
	got, flags, err := executeSurfaceShader(program, s, 65, 67, true, padded, make([]byte, 65*67*4))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, surfaceCPUReference(raw, s, 65, 67)) || len(flags) != 4 {
		t.Fatal("padded GPU source changed pixels")
	}
	for y := 0; y < 67; y++ {
		for x := 65 * 4; x < pitch; x++ {
			padded[y*pitch+x] ^= 255
		}
	}
	_, flags, err = executeSurfaceShader(program, s, 65, 67, false, padded, got)
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range flags {
		if flag != 0 {
			t.Fatal("source row padding marked pixels dirty")
		}
	}
}

func TestProgressiveSurfaceResidentWire(t *testing.T) {
	executable := shaderExecutable(t)
	for _, bits := range []int{0, 2} {
		t.Run(fmt.Sprintf("refine%d", bits), func(t *testing.T) {
			program := startShader(t, executable).(*shaderCompute)
			s := &gpuconv.Surface{SharedTexture: 1, Width: 129, Height: 131}
			fixture := &surfaceComputeFixture{program: program, frames: map[*gpuconv.Surface][]byte{s: progressivePixels(s.Width, s.Height, "gradient")}}
			gpu := &progressiveEncoder{native: &gpuProgressiveCodec{compute: fixture, quantization: 6}, required: true, first: true, quantization: 6, refineBits: bits}
			defer gpu.close()
			cpu := &progressiveEncoder{first: true, quantization: 6, refineBits: bits}
			for _, size := range [][2]int{{129, 131}, {93, 47}, {129, 131}} {
				w, h := size[0], size[1]
				for frame := 0; frame < 3; frame++ {
					if frame == 1 {
						fixture.frames[s][(128*129+128)*4] ^= 255
					}
					rects, err := gpu.prepareSurface(s, w, h, false)
					if err != nil {
						t.Fatal(err)
					}
					pixels := surfaceCPUReference(fixture.frames[s], s, w, h)
					for i := 0; i < len(rects); {
						end := progressiveBatchEnd(rects, i)
						got, err := gpu.encodeSurface(w, h, rects[i:end], uint32(frame))
						if err != nil {
							t.Fatal(err)
						}
						want, err := cpu.encode(pixels, w, h, rects[i:end], uint32(frame))
						if err != nil {
							t.Fatal(err)
						}
						if !bytes.Equal(got, want) {
							t.Fatal("resident encoding differs from CPU")
						}
						i = end
					}
					if len(rects) != 0 {
						if err := gpu.commitSurface(); err != nil {
							t.Fatal(err)
						}
					}
					if bits != 0 {
						for len(gpu.pendingRects(8)) != 0 {
							batch := gpu.pendingRects(8)
							got, err := gpu.upgrade(batch, 99)
							if err != nil {
								t.Fatal(err)
							}
							want, err := cpu.upgrade(batch, 99)
							if err != nil || !bytes.Equal(got, want) {
								t.Fatalf("resident UPGRADE differs: %v", err)
							}
						}
					}
				}
			}
			if fixture.uploads.Load() != 0 || fixture.resident.Load() == 0 {
				t.Fatal("surface encoding uploaded pixels")
			}
		})
	}
}

func TestProgressiveSurfaceBitmapLoop(t *testing.T) {
	executable := shaderExecutable(t)
	program := startShader(t, executable).(*shaderCompute)
	s := &gpuconv.Surface{SharedTexture: 1, Width: 65, Height: 67}
	fixture := &surfaceComputeFixture{program: program, frames: map[*gpuconv.Surface][]byte{s: progressivePixels(s.Width, s.Height, "gradient")}, prepared: make(chan struct{}, 4), committed: make(chan struct{}, 4)}
	encoder := &progressiveEncoder{native: &gpuProgressiveCodec{compute: fixture, quantization: 6}, required: true, first: true, quantization: 6}
	c := channelTestClient()
	c.codec = codecProgressive
	c.sent = 0
	c.conn = displayActivationConn{}
	c.frames = make(chan bridge.Frame, 1)
	freed := atomic.Int32{}
	f := bridge.Frame{Format: capture.FormatSurface, Surface: s, Width: s.Width, Height: s.Height, Buffer: framebuf.New(nil, func() { freed.Add(1) })}
	defer f.Release()
	finished := make(chan error, 1)
	go func() { finished <- c.bitmapLoopWithEncoder(encoder) }()
	t.Cleanup(func() {
		close(c.done)
		if err := <-finished; err != nil {
			t.Error(err)
		}
		if freed.Load() != 1 {
			t.Error("GPU bitmap loop leaked the held snapshot")
		}
	})
	c.frames <- f.Retain()
	select {
	case <-fixture.committed:
	case <-time.After(5 * time.Second):
		t.Fatal("GPU frame not committed")
	}
	c.frames <- f.Retain()
	for i := 0; i < 2; i++ {
		select {
		case <-fixture.prepared:
		case <-time.After(5 * time.Second):
			t.Fatal("GPU frame not prepared")
		}
	}
	if fixture.uploads.Load() != 0 || fixture.resident.Load() == 0 || fixture.commits.Load() != 1 {
		t.Fatal("bitmap loop read/uploaded pixels or resent an unchanged surface")
	}
}

func TestProgressiveSurfaceClientScaling(t *testing.T) {
	executable := shaderExecutable(t)
	program := startShader(t, executable).(*shaderCompute)
	s := &gpuconv.Surface{SharedTexture: 1, Width: 65, Height: 67}
	fixture := &surfaceComputeFixture{program: program, frames: map[*gpuconv.Surface][]byte{s: progressivePixels(s.Width, s.Height, "gradient")}, prepared: make(chan struct{}, 8), committed: make(chan struct{}, 8)}
	encoder := &progressiveEncoder{native: &gpuProgressiveCodec{compute: fixture, quantization: 6}, required: true, first: true, quantization: 6}
	conn := &surfaceTestConn{frames: make(chan struct{}, 8), allowScaled: true}
	c := channelTestClient()
	c.codec = codecProgressive
	c.sent = 0
	c.suspendAck = true
	c.scaledMapping = true
	c.conn = conn
	c.frames = make(chan bridge.Frame, 1)
	c.setOutputSize(400, 200)
	var freed atomic.Int32
	f := bridge.Frame{Format: capture.FormatSurface, Surface: s, Width: s.Width, Height: s.Height, Buffer: framebuf.New(nil, func() { freed.Add(1) })}
	result := make(chan error, 1)
	go func() { result <- c.bitmapLoopWithEncoder(encoder) }()
	defer func() {
		c.close()
		if err := <-result; err != nil {
			t.Error(err)
		}
		f.Release()
		if freed.Load() != 1 {
			t.Error("surface snapshot leaked")
		}
	}()
	wait := func(events <-chan struct{}) {
		t.Helper()
		select {
		case <-events:
		case err := <-result:
			result <- err
			t.Fatal("GPU bitmap failed", err)
		case <-time.After(5 * time.Second):
			t.Fatal("GPU bitmap not processed")
		}
	}
	c.frames <- f.Retain()
	wait(fixture.committed)
	assertScaledFrame(t, c, conn, 65, 67, 400, 200)
	c.frames <- f.Retain()
	wait(fixture.prepared)
	wait(fixture.prepared)
	if fixture.commits.Load() != 1 {
		t.Fatal("unchanged GPU frame resent")
	}
	c.setOutputSize(600, 400)
	c.frames <- f.Retain()
	wait(fixture.committed)
	assertScaledFrame(t, c, conn, 65, 67, 600, 400)
	if fixture.w != 65 || fixture.h != 67 || fixture.uploads.Load() != 0 || fixture.commits.Load() != 2 || conn.resets != 2 {
		t.Fatal("client resize resampled/read back capture pixels or skipped the forced repaint")
	}
}
