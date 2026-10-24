package gpuconv

import "fmt"

// ProgressiveMetadata validates the shared six-buffer compute ABI. Input is a
// batch of at most eight padded 64x64 BGRA/NV12 tiles, never a complete desktop.
// Pass 0 emits SIMPLE, 1 emits FIRST, 2 refines the cached FIRST coefficients.
// Metadata word 3 selects 0x100/0x101/0x102 for the 50%/75%/100% stages;
// values 1..4 retain the legacy uniform-precision fixture ABI.
func ProgressiveMetadata(input []byte, meta []uint32) (int, error) {
	if len(meta) != 16 || meta[0] < 1 || meta[0] > 8 || meta[1] > 1 || meta[2] < 6 || meta[2] > 15 ||
		meta[4] > 2 || meta[5] < 1 || meta[5] > (128<<20)/(12288*4) ||
		(meta[4] != 0 && !progressiveStageValid(meta[3], meta[4])) {
		return 0, fmt.Errorf("invalid Progressive compute metadata")
	}
	bytes := int(meta[0]) * 16384
	if meta[1] == 1 {
		bytes = int(meta[0]) * 6144
	}
	if meta[4] == 2 {
		bytes = 4
	}
	limit := meta[5]
	if meta[6] != 0 {
		width := meta[6] / 4
		if meta[6]%4 != 0 || width < 1 || width > 8192 || meta[7] < 1 || meta[7] > 8192 || uint64(meta[6])*uint64(meta[7]) > 64<<20 || meta[1] != 0 || meta[4] == 2 {
			return 0, fmt.Errorf("invalid resident Progressive geometry")
		}
		limit = ((width + 63) / 64) * ((meta[7] + 63) / 64)
		if meta[4] != 0 && meta[5] != limit {
			return 0, fmt.Errorf("invalid resident Progressive cache size")
		}
		bytes = 4
	}
	if len(input) != bytes {
		return 0, fmt.Errorf("invalid Progressive compute input size")
	}
	for i := uint32(0); i < meta[0]; i++ {
		if meta[8+i] >= limit {
			return 0, fmt.Errorf("invalid Progressive coefficient index")
		}
		for j := uint32(0); j < i; j++ {
			if meta[8+i] == meta[8+j] {
				return 0, fmt.Errorf("duplicate Progressive coefficient index")
			}
		}
	}
	return int(meta[5]) * 12288 * 4, nil
}

func progressiveStageValid(stage, pass uint32) bool {
	return stage >= 1 && stage <= 4 || pass == 1 && stage == 0x100 || pass == 2 && (stage == 0x101 || stage == 0x102)
}
