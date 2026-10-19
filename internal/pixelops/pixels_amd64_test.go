package pixelops

import "golang.org/x/sys/cpu"

func testImplementations() []implementation {
	all := []implementation{scalar, sse2}
	if cpu.X86.HasAVX2 {
		all = append(all, avx2)
	}
	return all
}
