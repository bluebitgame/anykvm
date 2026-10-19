//go:build cgo && amd64 && (darwin || linux || windows)

package pixelconv

import "golang.org/x/sys/cpu"

var sse2 = implementation{"SSE2", convertSSE2, deltaSSE2}
var avx2 = implementation{"AVX2", convertAVX2, deltaAVX2}

func init() {
	selected = sse2
	// HasAVX2 also checks OS support for XMM/YMM state. Optional instructions
	// stay inside functions compiled with target(avx2).
	if cpu.X86.HasAVX2 {
		selected = avx2
	}
}
func convertSSE2(dst, first, second []byte, fs, ss, w, h int, nv12 bool) {
	convertNative(0, dst, first, second, fs, ss, w, h, nv12)
}
func convertAVX2(dst, first, second []byte, fs, ss, w, h int, nv12 bool) {
	convertNative(1, dst, first, second, fs, ss, w, h, nv12)
}
func deltaSSE2(dst, row, prev []byte) { deltaNative(0, dst, row, prev) }
func deltaAVX2(dst, row, prev []byte) { deltaNative(1, dst, row, prev) }
