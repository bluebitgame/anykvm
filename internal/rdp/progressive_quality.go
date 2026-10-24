package rdp

// Quality profiles from Microsoft's WindowsProtocolTestSuites RdpegfxTileUtils:
// ProtoSDK/MS-RDPEGFX/Codecs/RdpegfxTileUtils.cs, gProgressiveBitPosArray.
// Entries are LL3, HL3, LH3, HH3, HL2, LH2, HH2, HL1, LH1, HH1.
const (
	progressiveFirst50    = 0x100
	progressiveUpgrade75  = 0x101
	progressiveUpgrade100 = 0x102
)

var progressiveQualityBits = [2][3][10]uint8{
	{
		{1, 1, 1, 1, 2, 2, 2, 1, 1, 1},
		{1, 1, 1, 1, 2, 2, 2, 1, 1, 1},
		{1, 1, 1, 1, 2, 2, 2, 1, 1, 0},
	},
	{
		{0, 1, 1, 1, 1, 1, 1, 0, 0, 0},
		{0, 1, 1, 1, 2, 1, 1, 0, 0, 0},
		{0, 1, 1, 1, 2, 1, 1, 1, 0, 0},
	},
}

func progressiveOfficialPass(bits int) bool {
	return bits >= progressiveFirst50 && bits <= progressiveUpgrade100
}

func validProgressivePass(bits int, upgrade bool) bool {
	if progressiveOfficialPass(bits) {
		return upgrade == (bits != progressiveFirst50)
	}
	return bits >= 0 && bits <= 4 && (!upgrade || bits != 0)
}

func progressiveBand(i int) int {
	ends := [...]int{1023, 2046, 3007, 3279, 3551, 3807, 3879, 3951, 4015, 4096}
	bands := [...]int{7, 8, 9, 4, 5, 6, 1, 2, 3, 0}
	for n, end := range ends {
		if i < end {
			return bands[n]
		}
	}
	panic("invalid Progressive coefficient index")
}

func progressiveQualityBit(stage, component, band int) int {
	if stage >= progressiveUpgrade100 {
		return 0
	}
	return int(progressiveQualityBits[stage-progressiveFirst50][component][band])
}

func progressiveQualityTable(stage int) []byte {
	quality := [...]byte{50, 75, 100}
	out := []byte{quality[stage-progressiveFirst50]}
	for c := 0; c < 3; c++ {
		for b := 0; b < 10; b += 2 {
			out = append(out, byte(progressiveQualityBit(stage, c, b)|progressiveQualityBit(stage, c, b+1)<<4))
		}
	}
	return out
}

// Non-LL coefficients use sign/magnitude progressive precision. LL3 uses an
// arithmetic shift and unsigned residual bits, as in the Microsoft encoder.
func progressiveCoarse(v int32, bits, component, index int) int32 {
	if progressiveOfficialPass(bits) {
		shift := progressiveQualityBit(bits, component, progressiveBand(index))
		if index >= progressiveReduceLL {
			return v >> shift
		}
		return v / int32(1<<shift)
	}
	if index < progressiveReduceLL {
		return v / int32(1<<bits)
	}
	return v
}

func encodeProgressiveQualityUpgrade(coeffs []int32, stage, component int) ([]byte, []byte) {
	var values [4096]int32
	var counts [4096]int
	n := 0
	raw := rfxBitWriter{}
	for i, v := range coeffs {
		band := progressiveBand(i)
		from := progressiveQualityBit(stage-1, component, band)
		to := progressiveQualityBit(stage, component, band)
		bits := from - to
		if bits == 0 {
			continue
		}
		mask := int32(1<<bits) - 1
		if i >= progressiveReduceLL {
			raw.bits(uint32((v>>to)&mask), bits)
			continue
		}
		magnitude := v
		if magnitude < 0 {
			magnitude = -magnitude
		}
		delta := (magnitude >> to) & mask
		if magnitude>>from != 0 {
			raw.bits(uint32(delta), bits)
		} else {
			if v < 0 {
				delta = -delta
			}
			values[n], counts[n] = delta, bits
			n++
		}
	}
	return encodeProgressiveVariableSRL(values[:n], counts[:n]), raw.data
}

func encodeProgressiveVariableSRL(values []int32, counts []int) []byte {
	w := rfxBitWriter{}
	kp := 8
	for i := 0; i < len(values); {
		run := 0
		for i < len(values) && values[i] == 0 {
			run++
			i++
		}
		k := kp >> 3
		for run >= 1<<k {
			w.bits(0, 1)
			run -= 1 << k
			kp = min(80, kp+4)
			k = kp >> 3
		}
		w.bits(1, 1)
		w.bits(uint32(run), k)
		kp = max(0, kp-6)
		if i == len(values) {
			break
		}
		v := values[i]
		maximum := int32(1<<counts[i]) - 1
		if v < 0 {
			w.bits(1, 1)
			v = -v
		} else {
			w.bits(0, 1)
		}
		for j := int32(1); j < v; j++ {
			w.bits(0, 1)
		}
		if v < maximum {
			w.bits(1, 1)
		}
		i++
	}
	return append(w.data, 0)
}
