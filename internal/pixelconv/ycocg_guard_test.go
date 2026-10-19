//go:build darwin || linux

package pixelconv

import (
	"golang.org/x/sys/unix"
	"testing"
)

func guarded(t *testing.T, size int) []byte {
	page := unix.Getpagesize()
	memory, err := unix.Mmap(-1, 0, page*3, unix.PROT_NONE, unix.MAP_PRIVATE|unix.MAP_ANON)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Munmap(memory) })
	if err = unix.Mprotect(memory[page:page*2], unix.PROT_READ|unix.PROT_WRITE); err != nil {
		t.Fatal(err)
	}
	return memory[page*2-size : page*2 : page*2]
}
func TestYCoCg420GuardPages(t *testing.T) {
	for _, impl := range testImplementations() {
		t.Run(impl.name, func(t *testing.T) {
			for w := 1; w <= 33; w++ {
				for _, h := range []int{1, 2, 3} {
					for _, nv := range []bool{false, true} {
						fs, ss := w*4, 2*((w+1)/2)
						if nv {
							fs = w
						}
						first, uv, dst := guarded(t, fs*h), guarded(t, ss*((h+1)/2)), guarded(t, Size420(w, h))
						impl.convert(dst, first, uv, fs, ss, w, h, nv)
					}
				}
			}
			for size := 1; size <= 65; size++ {
				impl.delta(guarded(t, size), guarded(t, size), guarded(t, size))
			}
		})
	}
}
