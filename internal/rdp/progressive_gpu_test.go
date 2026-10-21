package rdp

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

type shaderCompute struct {
	input   io.WriteCloser
	output  io.Reader
	command *exec.Cmd
	stderr  bytes.Buffer
}

func (s *shaderCompute) Run(input []byte, meta []uint32) ([]byte, [48]uint32, error) {
	var lengths [48]uint32
	request := new(bytes.Buffer)
	_ = binary.Write(request, binary.LittleEndian, meta)
	_ = binary.Write(request, binary.LittleEndian, uint32(len(input)))
	request.Write(input)
	if _, err := s.input.Write(request.Bytes()); err != nil {
		return nil, lengths, err
	}
	var size uint32
	if err := binary.Read(s.output, binary.LittleEndian, &size); err != nil {
		return nil, lengths, fmt.Errorf("shader emulator: %w: %s", err, s.stderr.String())
	}
	if size > 48*32768 {
		return nil, lengths, fmt.Errorf("invalid shader size %d", size)
	}
	if err := binary.Read(s.output, binary.LittleEndian, &lengths); err != nil {
		return nil, lengths, err
	}
	out := make([]byte, size)
	_, err := io.ReadFull(s.output, out)
	return out, lengths, err
}
func (s *shaderCompute) Close() { _ = s.input.Close(); _ = s.command.Wait() }

func shaderExecutable(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("CPU HLSL execution fixture uses a POSIX C++ compiler; use ANYKVM_TEST_RDP_GPU=1 for D3D11")
	}
	compiler, err := exec.LookPath("c++")
	if err != nil {
		t.Skip("C++ compiler unavailable for CPU execution of integer HLSL")
	}
	dir := t.TempDir()
	// Strip only GPU binding/parameter syntax. All algorithm code comes directly
	// from the shipped HLSL, with 64 real threads honoring its group barriers.
	source := regexp.MustCompile(`(?m)^BINDING\([^\n]+\n`).ReplaceAllString(progressiveShader, "")
	source = regexp.MustCompile(`\[numthreads\([^\]]+\)\]`).ReplaceAllString(source, "")
	source = strings.NewReplacer("groupshared ", "", "inout Writer w", "Writer &w", "inout int krp", "int &krp",
		"void main(", "void transformTile(", " : SV_GroupID", "", " : SV_GroupIndex", "", " : SV_DispatchThreadID", "").Replace(source)
	if err := os.WriteFile(filepath.Join(dir, "shader.hpp"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	surface := regexp.MustCompile(`(?m)^BIND\([^\n]+\n`).ReplaceAllString(progressiveSurfaceShader, "")
	surface = regexp.MustCompile(`\[numthreads\([^\]]+\)\]`).ReplaceAllString(surface, "")
	surface = strings.NewReplacer("groupshared uint groupDirty", "std::atomic<uint> groupDirty", "out uint ", "uint &",
		"void main(", "void prepareTile(", " : SV_GroupID", "", " : SV_GroupIndex", "").Replace(surface)
	if err := os.WriteFile(filepath.Join(dir, "surface.hpp"), []byte(surface), 0600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(dir, "progressive-shader")
	command := exec.Command(compiler, "-std=c++17", "-O2", "-pthread", "-I"+dir, "testdata/progressive_shader.cpp", "-o", executable)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile HLSL CPU fixture: %v\n%s", err, output)
	}
	return executable
}
func startShader(t *testing.T, executable string) progressiveCompute {
	t.Helper()
	s := &shaderCompute{command: exec.Command(executable)}
	var err error
	s.input, err = s.command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	s.output, err = s.command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	s.command.Stderr = &s.stderr
	if err = s.command.Start(); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestProgressiveShaderWireParity(t *testing.T) {
	executable := shaderExecutable(t)
	testProgressiveComputeParity(t, func(t *testing.T) progressiveCompute { return startShader(t, executable) })
}

// Opt-in driver execution. Forced GPU mode must fail, rather than silently
// validating a CPU fallback, when hardware or shader compilation is unavailable.
func TestProgressiveGPUWireParity(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_RDP_GPU") != "1" {
		t.Skip("set ANYKVM_TEST_RDP_GPU=1 on a Linux Vulkan or Windows D3D11 GPU host")
	}
	testProgressiveComputeParity(t, func(t *testing.T) progressiveCompute {
		native, err := newGPUProgressive("gpu", 6)
		if err != nil {
			t.Fatal(err)
		}
		return native.(*gpuProgressiveCodec).compute
	})
}

func testProgressiveComputeParity(t *testing.T, newCompute func(*testing.T) progressiveCompute) {
	for _, q := range []int{6, 10, 15} {
		for _, bits := range []int{0, 1, 2, 3, 4} {
			t.Run(fmt.Sprintf("q%d/refine%d", q, bits), func(t *testing.T) {
				compute := newCompute(t)
				gpu := &progressiveEncoder{native: &gpuProgressiveCodec{compute: compute, quantization: q}, required: true, first: true, quantization: q, refineBits: bits}
				defer gpu.close()
				cpu := &progressiveEncoder{first: true, quantization: q, refineBits: bits}
				for _, format := range []progressivePixelFormat{progressiveBGRA, progressiveNV12} {
					for _, size := range [][2]int{{1, 1}, {65, 67}, {64, 64}} {
						w, h := size[0], size[1]
						for frame, pattern := range []string{"solid", "noise", "edge"} {
							p := progressivePixels(w, h, pattern)
							if format == progressiveNV12 {
								p = progressiveNV12Pixels(w, h, pattern)
							}
							rects := dirtyProgressiveRectsFormat(p, nil, w, h, format)
							got, err := gpu.encodeFormat(p, w, h, rects, uint32(frame), format)
							if err != nil {
								t.Fatal(err)
							}
							want, err := cpu.encodeFormat(p, w, h, rects, uint32(frame), format)
							if err != nil {
								t.Fatal(err)
							}
							if !bytes.Equal(got, want) {
								t.Fatalf("FIRST/SIMPLE mismatch format=%d size=%v pattern=%s: got %d want %d", format, size, pattern, len(got), len(want))
							}
							if bits != 0 {
								if len(gpu.coefficients) != 0 {
									t.Fatal("GPU coefficients allocated in host memory")
								}
								pending := gpu.pendingRects(8)
								got, err = gpu.upgrade(pending, 99)
								if err != nil {
									t.Fatal(err)
								}
								want, err = cpu.upgrade(pending, 99)
								if err != nil {
									t.Fatal(err)
								}
								if !bytes.Equal(got, want) {
									t.Fatalf("UPGRADE mismatch format=%d size=%v pattern=%s: got %d want %d", format, size, pattern, len(got), len(want))
								}
							}
						}
					}
				}
			})
		}
	}
	t.Run("cache-lifecycle", func(t *testing.T) {
		compute := newCompute(t)
		gpu := &progressiveEncoder{native: &gpuProgressiveCodec{compute: compute, quantization: 6}, required: true, first: true, quantization: 6, refineBits: 2}
		defer gpu.close()
		cpu := &progressiveEncoder{first: true, quantization: 6, refineBits: 2}
		const w, h = 129, 131 // Nine tiles span more than one GPU batch.
		p := progressivePixels(w, h, "gradient")
		rects := dirtyProgressiveRects(p, nil, w, h)
		encode := func(pixels []byte, batch []bitmapRect, frame uint32) {
			got, err := gpu.encode(pixels, w, h, batch, frame)
			if err != nil {
				t.Fatal(err)
			}
			want, err := cpu.encode(pixels, w, h, batch, frame)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("cache FIRST mismatch: %v", err)
			}
		}
		encode(p, rects[:8], 1)
		encode(p, rects[8:], 1)
		// Replace a pending FIRST before any UPGRADE. Only the newest cache
		// entry may be refined; the rest of the earlier batches must survive.
		encode(progressivePixels(w, h, "noise"), rects[:1], 2)
		for len(gpu.pendingRects(8)) != 0 {
			pending := gpu.pendingRects(8)
			got, err := gpu.upgrade(pending, 3)
			if err != nil {
				t.Fatal(err)
			}
			want, err := cpu.upgrade(pending, 3)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("cache UPGRADE mismatch: %v", err)
			}
		}
		encode(p, rects[:1], 4)
		gpu.discardRefinements()
		cpu.discardRefinements()
		if _, err := gpu.upgrade(rects[:1], 5); err == nil {
			t.Fatal("refined an invalidated FIRST")
		}
		encode(p, rects[8:], 6)
		got, err := gpu.upgrade(rects[8:], 7)
		if err != nil {
			t.Fatal(err)
		}
		want, err := cpu.upgrade(rects[8:], 7)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("refresh cache mismatch: %v", err)
		}
	})
}

func TestProgressiveGPUReadbackBounds(t *testing.T) {
	var lengths [48]uint32
	lengths[0], lengths[2], lengths[4] = 1, 1, 1
	if _, err := progressiveGPUStreams(make([]byte, 12), lengths, 1, false); err != nil {
		t.Fatal(err)
	}
	for _, n := range []uint32{0, 32769, 0xffffffff} {
		lengths[0] = n
		if _, err := progressiveGPUStreams(make([]byte, 12), lengths, 1, false); err == nil {
			t.Fatalf("accepted length %d", n)
		}
	}
	lengths[0] = 1
	lengths[1] = 1
	if _, err := progressiveGPUStreams(make([]byte, 16), lengths, 1, false); err == nil {
		t.Fatal("accepted raw stream in SIMPLE")
	}
}
