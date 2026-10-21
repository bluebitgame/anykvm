// Package pixelops provides shared operations on opaque desktop BGRA pixels.
// Color comparisons ignore the unused alpha byte. Buffers are tightly packed
// unless a row stride is supplied explicitly.
package pixelops

const BlockSize = 64

// Block is a changed rectangle in pixels, clipped at the picture's edges.
type Block struct{ X, Y, W, H int }

type implementation struct {
	name    string
	equal   func(a, b []byte, stride, width, height int) bool
	run     func(pixels []byte) int
	pack    func(dst, src []byte)
	runByte func(pixels []byte) int
}

var scalar = implementation{"Go", equalBlockGo, runBGRGo, packBGRGo, runByteGo}
var selected = scalar

// Backend reports the implementation used for bulk operations. Short runs and
// small rows may use Go to avoid dispatch overhead.
func Backend() string { return selected.name }

// EqualBGR compares complete BGRA pixels while ignoring alpha.
func EqualBGR(a, b []byte) bool {
	if len(a)%4 != 0 || len(b)%4 != 0 {
		panic("pixelops: incomplete BGRA pixel")
	}
	if len(a) != len(b) {
		return false
	}
	return EqualBGRBlock(a, b, len(a), len(a)/4, 1)
}

// EqualBGRBlock compares width x height pixels, beginning at the start of each
// buffer. Stride is the byte distance between rows in both buffers. Padding and
// alpha are ignored. Neither buffer is modified.
func EqualBGRBlock(a, b []byte, stride, width, height int) bool {
	if width < 0 || height < 0 || stride < 0 || width > stride/4 {
		panic("pixelops: invalid block geometry")
	}
	if width == 0 || height == 0 {
		return true
	}
	rowBytes := width * 4
	if len(a) < rowBytes || len(b) < rowBytes ||
		height-1 > (len(a)-rowBytes)/stride || height-1 > (len(b)-rowBytes)/stride {
		panic("pixelops: truncated block")
	}
	if width < 4 {
		return equalBlockGo(a, b, stride, width, height)
	}
	return selected.equal(a, b, stride, width, height)
}

// RunBGR returns the number of leading pixels with the first pixel's BGR color.
// An empty buffer has a run of zero. Alpha differences do not end the run.
func RunBGR(pixels []byte) int {
	if len(pixels)%4 != 0 {
		panic("pixelops: incomplete BGRA pixel")
	}
	// Most noisy-image runs have one pixel. Avoid a SIMD call in that case.
	if len(pixels) < 64 || pixels[0] != pixels[4] || pixels[1] != pixels[5] || pixels[2] != pixels[6] {
		return runBGRGo(pixels)
	}
	return selected.run(pixels)
}

// PackBGR strips alpha into dst and returns its written prefix. Dst must hold
// three bytes per source pixel, and must not overlap src. No allocation occurs.
func PackBGR(dst, src []byte) []byte {
	if len(src)%4 != 0 || len(dst) < len(src)/4*3 {
		panic("pixelops: invalid BGR packing buffers")
	}
	dst = dst[:len(src)/4*3]
	if len(src) < 64 {
		packBGRGo(dst, src)
	} else {
		selected.pack(dst, src)
	}
	return dst
}

// RunByte returns the number of leading equal bytes, for planar RLE encoders.
// Short or immediately differing runs avoid the SIMD dispatch cost.
func RunByte(p []byte) int {
	if len(p) < 32 || p[0] != p[1] {
		return runByteGo(p)
	}
	return selected.runByte(p)
}

func runByteGo(p []byte) int {
	if len(p) == 0 {
		return 0
	}
	i := 1
	for i < len(p) && p[i] == p[0] {
		i++
	}
	return i
}

// DirtyBlocks compares 64x64 blocks against the last transmitted picture.
// A missing or differently sized baseline marks every block dirty. Tolerance
// is the maximum |dB|+|dG|+|dR| per pixel; zero uses exact SIMD comparison.
// Results are in row order, with partial edge blocks clipped to the picture.
func DirtyBlocks(previous, pixels []byte, width, height, tolerance int) []Block {
	return dirtyBlocks(previous, pixels, width, height, tolerance, nil, false)
}

// DirtyBlocksCandidates compares only sorted, unique 64px tile IDs. An empty
// list skips comparison. Malformed IDs conservatively fall back to all blocks.
func DirtyBlocksCandidates(previous, pixels []byte, width, height, tolerance int, candidates []uint32) []Block {
	return dirtyBlocks(previous, pixels, width, height, tolerance, candidates, true)
}

func dirtyBlocks(previous, pixels []byte, width, height, tolerance int, candidates []uint32, filtered bool) []Block {
	if width <= 0 || height <= 0 || width > len(pixels)/4 ||
		height != len(pixels)/4/width || len(pixels)%4 != 0 || len(pixels)/4%width != 0 {
		panic("pixelops: invalid BGRA picture")
	}
	var blocks []Block
	stride := width * 4
	refresh := len(previous) != len(pixels)
	cols, rows := (width+BlockSize-1)/BlockSize, (height+BlockSize-1)/BlockSize
	for i, id := range candidates {
		if uint64(id) >= uint64(cols*rows) || (i > 0 && id <= candidates[i-1]) {
			filtered = false
			break
		}
	}
	if refresh {
		filtered = false
	}
	count := cols * rows
	if filtered {
		count = len(candidates)
	}
	for i := 0; i < count; i++ {
		id := i
		if filtered {
			id = int(candidates[i])
		}
		x, y := (id%cols)*BlockSize, (id/cols)*BlockSize

		b := Block{x, y, min(BlockSize, width-x), min(BlockSize, height-y)}
		dirty := refresh
		if !dirty {
			offset := y*stride + x*4
			if tolerance <= 0 {
				dirty = !EqualBGRBlock(pixels[offset:], previous[offset:], stride, b.W, b.H)
			} else {
				dirty = !equalBlockTolerance(pixels[offset:], previous[offset:], stride, b.W, b.H, tolerance)
			}
		}
		if dirty {
			blocks = append(blocks, b)
		}
	}
	return blocks
}

func equalBlockGo(a, b []byte, stride, width, height int) bool {
	for y := 0; y < height; y++ {
		row := y * stride
		for i := row; i < row+width*4; i += 4 {
			if a[i] != b[i] || a[i+1] != b[i+1] || a[i+2] != b[i+2] {
				return false
			}
		}
	}
	return true
}

func runBGRGo(pixels []byte) int {
	if len(pixels) == 0 {
		return 0
	}
	b, g, r := pixels[0], pixels[1], pixels[2]
	i := 4
	for i < len(pixels) && pixels[i] == b && pixels[i+1] == g && pixels[i+2] == r {
		i += 4
	}
	return i / 4
}

func packBGRGo(dst, src []byte) {
	for i, j := 0, 0; i < len(src); i, j = i+4, j+3 {
		dst[j], dst[j+1], dst[j+2] = src[i], src[i+1], src[i+2]
	}
}

func equalBlockTolerance(a, b []byte, stride, width, height, tolerance int) bool {
	for y := 0; y < height; y++ {
		for i := y * stride; i < y*stride+width*4; i += 4 {
			if absDifference(a[i], b[i])+absDifference(a[i+1], b[i+1])+absDifference(a[i+2], b[i+2]) > tolerance {
				return false
			}
		}
	}
	return true
}

func absDifference(a, b byte) int {
	if a > b {
		return int(a) - int(b)
	}
	return int(b) - int(a)
}
