package pixelconv

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestYCoCg420Kernels(t *testing.T) {
	rng := rand.New(rand.NewSource(23452))
	for _, impl := range testImplementations() {
		t.Run(impl.name, func(t *testing.T) {
			for _, w := range []int{1, 2, 3, 4, 7, 8, 15, 16, 17, 31, 32, 33, 63, 64, 65, 127, 129} {
				for _, h := range []int{1, 2, 3, 17} {
					for _, nv12 := range []bool{false, true} {
						for _, offset := range []int{0, 1, 7, 15} {
							fs, ss := w*4+13, 2*((w+1)/2)+11
							if nv12 {
								fs = w + 13
							}
							first := make([]byte, fs*h+offset)
							second := make([]byte, ss*((h+1)/2)+offset)
							rng.Read(first)
							rng.Read(second)
							original := append([]byte(nil), first...)
							originalUV := append([]byte(nil), second...)
							want := make([]byte, Size420(w, h))
							convertGo(want, first[offset:], second[offset:], fs, ss, w, h, nv12)
							memory := bytes.Repeat([]byte{0x59}, len(want)+offset+32)
							got := memory[offset : offset+len(want)]
							impl.convert(got, first[offset:], second[offset:], fs, ss, w, h, nv12)
							if !bytes.Equal(got, want) {
								t.Fatalf("%dx%d nv12=%t offset=%d differs", w, h, nv12, offset)
							}
							if !bytes.Equal(memory[:offset], bytes.Repeat([]byte{0x59}, offset)) || !bytes.Equal(memory[offset+len(want):], bytes.Repeat([]byte{0x59}, 32)) {
								t.Fatal("output overrun")
							}
							if !bytes.Equal(first, original) || !bytes.Equal(second, originalUV) {
								t.Fatal("input modified")
							}
						}
					}
				}
			}
			for _, size := range []int{0, 1, 2, 7, 15, 16, 17, 31, 32, 33, 127, 128, 129, 16384} {
				row, prior := make([]byte, size), make([]byte, size)
				rng.Read(row)
				rng.Read(prior)
				want, got := make([]byte, size), make([]byte, size)
				deltaGo(want, row, prior)
				impl.delta(got, row, prior)
				if !bytes.Equal(got, want) {
					t.Fatalf("delta size=%d differs", size)
				}
				copy(got, row)
				impl.delta(got, got, prior)
				if !bytes.Equal(got, want) {
					t.Fatal("in-place delta differs")
				}
			}
		})
	}
}

func TestYCoCg420PublicBuffers(t *testing.T) {
	const w, h = 3, 3
	src := bytes.Repeat([]byte{10, 30, 210, 255}, w*h)
	want := append(bytes.Repeat([]byte{70}, w*h), bytes.Repeat([]byte{156}, 4)...)
	want = append(want, bytes.Repeat([]byte{216}, 4)...)
	if got := BGRAToYCoCg420(make([]byte, Size420(w, h)), src, w*4, w, h); !bytes.Equal(got, want) {
		t.Fatalf("solid color planes: %x", got)
	}
	for _, fn := range []func(){
		func() { Size420(0, 1) }, func() { Size420(8193, 1) },
		func() { BGRAToYCoCg420(make([]byte, 17), src, 11, 3, 3) },
		func() { BGRAToYCoCg420(make([]byte, 16), src, 12, 3, 3) },
		func() { NV12ToYCoCg420(make([]byte, 17), make([]byte, 9), make([]byte, 7), 3, 4, 3, 3) },
		func() { Delta(make([]byte, 1), make([]byte, 2), make([]byte, 2)) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("invalid buffer accepted")
				}
			}()
			fn()
		}()
	}
}
