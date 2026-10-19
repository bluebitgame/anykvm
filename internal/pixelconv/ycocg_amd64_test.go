//go:build cgo && amd64 && (darwin || linux || windows)

package pixelconv

import "golang.org/x/sys/cpu"

func testImplementations() []implementation {
	impls := []implementation{scalar, sse2}
	if cpu.X86.HasAVX2 {
		impls = append(impls, avx2)
	}
	return impls
}
