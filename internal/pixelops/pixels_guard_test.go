//go:build darwin || linux

package pixelops

import (
	"testing"

	"golang.org/x/sys/unix"
)

func guardedBuffer(t *testing.T, size int) []byte {
	t.Helper()
	page := unix.Getpagesize()
	memory, err := unix.Mmap(-1, 0, page*3, unix.PROT_NONE, unix.MAP_PRIVATE|unix.MAP_ANON)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Munmap(memory) })
	if err := unix.Mprotect(memory[page:page*2], unix.PROT_READ|unix.PROT_WRITE); err != nil {
		t.Fatal(err)
	}
	return memory[page*2-size : page*2 : page*2]
}

func TestPixelGuardPages(t *testing.T) {
	for _, impl := range testImplementations() {
		t.Run(impl.name, func(t *testing.T) {
			for n := 1; n <= 33; n++ {
				src, other, dst := guardedBuffer(t, n*4), guardedBuffer(t, n*4), guardedBuffer(t, n*3)
				if !impl.equal(src, other, n*4, n, 1) || impl.run(src) != n {
					t.Fatal("guarded pixel comparison changed")
				}
				impl.pack(dst, src)
				if impl.runByte(src) != len(src) {
					t.Fatal("guarded byte run changed")
				}
				src[len(src)-2] = 1
				if impl.equal(src, other, n*4, n, 1) {
					t.Fatal("guarded comparison missed the last pixel")
				}
			}
		})
	}
}
