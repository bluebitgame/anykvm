package pixelops

import "golang.org/x/sys/cpu"

var sse2 = implementation{"SSE2", equalBlockSSE2, runBGRSSE2, packBGRSSE2, runByteSSE2}
var avx2 = implementation{"AVX2", equalBlockAVX2, runBGRAVX2, packBGRAVX2, runByteAVX2}

func init() {
	// SSE2 is part of the AMD64 baseline. HasAVX2 also checks OS support for
	// saving XMM/YMM state. Optional instructions remain in separate routines.
	selected = sse2
	if cpu.X86.HasAVX2 {
		selected = avx2
	}
}

//go:noescape
func equalBlockSSE2(a, b []byte, stride, width, height int) bool

//go:noescape
func equalBlockAVX2(a, b []byte, stride, width, height int) bool

//go:noescape
func runBGRSSE2(pixels []byte) int

//go:noescape
func runBGRAVX2(pixels []byte) int

//go:noescape
func packBGRSSE2(dst, src []byte)

//go:noescape
func packBGRAVX2(dst, src []byte)

//go:noescape
func runByteSSE2(p []byte) int

//go:noescape
func runByteAVX2(p []byte) int
