package rdp

import (
	"bytes"
	"errors"
	"testing"
)

var officialProgressiveTables = [][]byte{
	{50, 0x11, 0x11, 0x22, 0x12, 0x11, 0x11, 0x11, 0x22, 0x12, 0x11, 0x11, 0x11, 0x22, 0x12, 0x01},
	{75, 0x10, 0x11, 0x11, 0x01, 0x00, 0x10, 0x11, 0x12, 0x01, 0x00, 0x10, 0x11, 0x12, 0x11, 0x00},
	{100, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
}

func assertProgressiveQualityWire(t *testing.T, packet []byte, stage int) {
	t.Helper()
	for len(packet) >= 6 {
		n := int(le.Uint32(packet[2:]))
		if n < 6 || n > len(packet) {
			t.Fatal("invalid Progressive block")
		}
		if le.Uint16(packet) == 0xccc4 {
			if packet[10] != 1 {
				t.Fatal("missing stage quantization table")
			}
			offset := 18 + 8*int(le.Uint16(packet[7:])) + 5*int(packet[9])
			if !bytes.Equal(packet[offset:offset+16], officialProgressiveTables[stage]) {
				t.Fatalf("stage %d quantization: %x", stage, packet[offset:offset+16])
			}
			tile := packet[offset+16 : n]
			want := uint16(0xccc7)
			qualityOffset := 13
			if stage == 0 {
				want = 0xccc6
				qualityOffset = 14
			}
			if le.Uint16(tile) != want || tile[qualityOffset] != 0 {
				t.Fatal("incorrect tile stage or quality-table index")
			}
			return
		}
		packet = packet[n:]
	}
	t.Fatal("missing Progressive region")
}

func TestProgressiveOfficialThreeStages(t *testing.T) {
	for _, backend := range []string{"go", "native-scalar", "native-sse2", "native-avx2"} {
		t.Run(backend, func(t *testing.T) {
			e := &progressiveEncoder{first: true, quantization: 6, refineBits: 2, required: true}
			defer e.close()
			if backend != "go" {
				native, err := newNativeProgressive(backend, 6)
				if errors.Is(err, errProgressiveSIMDUnavailable) || errors.Is(err, errProgressiveNativeUnavailable) {
					t.Skip(err)
				}
				if err != nil {
					t.Fatal(err)
				}
				e.native = native
			}
			oracle := &progressiveEncoder{first: true, quantization: 6, refineBits: 2}
			defer oracle.close()
			rects := []bitmapRect{{0, 0, 64, 64}}
			pixels := progressivePixels(64, 64, "noise")
			for stage := 0; stage < 3; stage++ {
				var got, want []byte
				var err error
				if stage == 0 {
					got, err = e.encode(pixels, 64, 64, rects, 1)
					if err == nil {
						want, err = oracle.encode(pixels, 64, 64, rects, 1)
					}
				} else {
					got, err = e.upgrade(rects, uint32(stage+1))
					if err == nil {
						want, err = oracle.upgrade(rects, uint32(stage+1))
					}
				}
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("stage %d: bytes=%d expected=%d error=%v", stage, len(got), len(want), err)
				}
				assertProgressiveQualityWire(t, got, stage)
				if e.pending[0] != (stage < 2) {
					t.Fatal("tile pending state does not track all three passes")
				}
			}
			if _, err := e.upgrade(rects, 4); err == nil {
				t.Fatal("fourth pass accepted")
			}
			if _, err := e.encode(pixels, 64, 64, rects, 5); err != nil {
				t.Fatal(err)
			}
			if e.tileStage(0) != 1 {
				t.Fatal("replacement did not restart at FIRST 50%")
			}
		})
	}
}

func TestProgressiveOfficialGPUStages(t *testing.T) {
	gpu := &progressiveEncoder{native: &gpuProgressiveCodec{compute: startShader(t, shaderExecutable(t)), quantization: 6}, required: true, first: true, quantization: 6, refineBits: 2}
	defer gpu.close()
	oracle := &progressiveEncoder{first: true, quantization: 6, refineBits: 2}
	defer oracle.close()
	pixels := progressivePixels(64, 64, "noise")
	rects := []bitmapRect{{0, 0, 64, 64}}
	for stage := 0; stage < 3; stage++ {
		var got, want []byte
		var err error
		if stage == 0 {
			got, err = gpu.encode(pixels, 64, 64, rects, 1)
			if err == nil {
				want, err = oracle.encode(pixels, 64, 64, rects, 1)
			}
		} else {
			got, err = gpu.upgrade(rects, uint32(stage+1))
			if err == nil {
				want, err = oracle.upgrade(rects, uint32(stage+1))
			}
		}
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("GPU stage %d mismatch: %v", stage, err)
		}
		assertProgressiveQualityWire(t, got, stage)
	}
}

type progressiveTestBits struct {
	data   []byte
	offset int
}

func (r *progressiveTestBits) read(t *testing.T, n int) uint32 {
	t.Helper()
	var value uint32
	for range n {
		if r.offset/8 >= len(r.data) {
			t.Fatal("truncated Progressive residual")
		}
		value = value<<1 | uint32((r.data[r.offset/8]>>uint(7-r.offset%8))&1)
		r.offset++
	}
	return value
}

// Decode SRL independently of the encoder, using the published run/sign/unary
// rules and the externally specified bit count for each participating element.
func decodeProgressiveTestSRL(t *testing.T, data []byte, counts []int) []int32 {
	r := progressiveTestBits{data: data}
	values := make([]int32, len(counts))
	kp := 8
	for i := 0; i < len(values); {
		run := 0
		k := kp >> 3
		for r.read(t, 1) == 0 {
			run += 1 << k
			kp = min(80, kp+4)
			k = kp >> 3
		}
		run += int(r.read(t, k))
		kp = max(0, kp-6)
		i += run
		if i > len(values) {
			t.Fatal("SRL zero run exceeds coefficient count")
		}
		if i == len(values) {
			break
		}
		negative := r.read(t, 1) != 0
		maximum := (1 << counts[i]) - 1
		v := 1
		for v < maximum && r.read(t, 1) == 0 {
			v++
		}
		if negative {
			v = -v
		}
		values[i] = int32(v)
		i++
	}
	return values
}

func TestProgressiveQualityResidualRoundTrip(t *testing.T) {
	// Exercise every band, signs first becoming significant at 75%/100%, and
	// negative LL3 values whose residual is unsigned (arithmetic FIRST shift).
	ends := []int{1023, 2046, 3007, 3279, 3551, 3807, 3879, 3951, 4015, 4096}
	bands := []int{7, 8, 9, 4, 5, 6, 1, 2, 3, 0}
	bit := func(stage, c, i int) int {
		band := 0
		for n, end := range ends {
			if i < end {
				band = bands[n]
				break
			}
		}
		return int(officialProgressiveTables[stage][1+c*5+band/2] >> uint(4*(band%2)) & 15)
	}
	for c := 0; c < 3; c++ {
		full, current := make([]int32, 4096), make([]int32, 4096)
		for i := range full {
			v := int32((i*37)%29 - 14)
			full[i] = v
			n := bit(0, c, i)
			if i >= 4015 {
				current[i] = v >> n << n
			} else {
				current[i] = v / int32(1<<n) * int32(1<<n)
			}
		}
		for stage := 1; stage <= 2; stage++ {
			srl, raw := encodeProgressiveQualityUpgrade(full, progressiveFirst50+stage, c)
			var counts []int
			for i, v := range current {
				n := bit(stage-1, c, i) - bit(stage, c, i)
				if i < 4015 && n > 0 && v == 0 {
					counts = append(counts, n)
				}
			}
			values := decodeProgressiveTestSRL(t, srl, counts)
			next := 0
			r := progressiveTestBits{data: raw}
			for i, v := range current {
				to := bit(stage, c, i)
				n := bit(stage-1, c, i) - to
				if n == 0 {
					continue
				}
				var delta int32
				if i >= 4015 || v != 0 {
					delta = int32(r.read(t, n))
					if i < 4015 && v < 0 {
						delta = -delta
					}
				} else {
					delta = values[next]
					next++
				}
				current[i] += delta << to
				want := full[i] >> to << to
				if i < 4015 {
					want = full[i] / int32(1<<to) * int32(1<<to)
				}
				if current[i] != want {
					t.Fatalf("component %d stage %d coefficient %d: %d want %d", c, stage, i, current[i], want)
				}
			}
		}
	}
}
