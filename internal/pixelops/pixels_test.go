package pixelops

import (
	"bytes"
	"math/rand"
	"reflect"
	"testing"
)

func TestPixelImplementations(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for _, impl := range testImplementations() {
		t.Run(impl.name, func(t *testing.T) {
			for offset := 0; offset < 32; offset++ {
				for _, n := range []int{0, 1, 2, 3, 4, 5, 7, 8, 9, 15, 16, 17, 31, 32, 33, 63, 64, 65, 129, 4096} {
					storage := make([]byte, offset+n*4)
					_, _ = rng.Read(storage)
					pixels := storage[offset:]
					before := append([]byte(nil), pixels...)
					want := make([]byte, 0, n*3)
					for i, v := range pixels {
						if i%4 != 3 {
							want = append(want, v)
						}
					}
					out := bytes.Repeat([]byte{0xa5}, offset+n*3+32)
					impl.pack(out[offset:offset+n*3], pixels)
					if !bytes.Equal(out[offset:offset+n*3], want) || !bytes.Equal(pixels, before) ||
						!bytes.Equal(out[:offset], bytes.Repeat([]byte{0xa5}, offset)) ||
						!bytes.Equal(out[offset+n*3:], bytes.Repeat([]byte{0xa5}, 32)) {
						t.Fatalf("pack n=%d offset=%d corrupted data or a boundary", n, offset)
					}
					other := append([]byte(nil), pixels...)
					for i := 3; i < len(other); i += 4 {
						other[i] ^= 255
					}
					if !impl.equal(pixels, other, n*4, n, 1) {
						t.Fatalf("alpha affected equality n=%d offset=%d", n, offset)
					}
					for _, index := range []int{0, n / 2, n - 1} {
						if n == 0 {
							break
						}
						for component := 0; component < 3; component++ {
							i := index*4 + component
							other[i] ^= 1
							if impl.equal(pixels, other, n*4, n, 1) {
								t.Fatalf("missed changed color n=%d pixel=%d component=%d", n, index, component)
							}
							other[i] ^= 1
						}
					}
					for i := 0; i < len(pixels); i += 4 {
						pixels[i], pixels[i+1], pixels[i+2] = 1, 17, 193
					}
					if got := impl.run(pixels); got != n {
						t.Fatalf("solid run=%d want=%d", got, n)
					}
					for _, stop := range []int{0, 1, 3, 4, 7, 8, 15, 16, 31, 32, n - 1} {
						if stop < 1 || stop >= n {
							continue
						}
						pixels[stop*4+2] ^= 1
						if got := impl.run(pixels); got != stop {
							t.Fatalf("run=%d want=%d n=%d offset=%d", got, stop, n, offset)
						}
						pixels[stop*4+2] ^= 1
					}
				}
			}
		})
	}
}

func TestPixelBlockStrides(t *testing.T) {
	for _, impl := range testImplementations() {
		for _, width := range []int{1, 3, 4, 7, 8, 9, 63, 64} {
			for _, height := range []int{1, 3, 64} {
				stride := width*4 + 13
				n := (height-1)*stride + width*4
				a, b := make([]byte, n), make([]byte, n)
				for y := 0; y < height; y++ {
					for x := 0; x < width; x++ {
						i := y*stride + x*4
						a[i+3] = 255
					}
					if y+1 < height {
						for i := y*stride + width*4; i < (y+1)*stride; i++ {
							a[i] = 99
						}
					}
				}
				if !impl.equal(a, b, stride, width, height) {
					t.Fatalf("%s compared alpha/padding at %dx%d", impl.name, width, height)
				}
				for _, index := range []int{0, (height/2)*stride + (width/2)*4, n - 4} {
					b[index+1] = 1
					if impl.equal(a, b, stride, width, height) {
						t.Fatalf("%s missed a changed pixel at %dx%d", impl.name, width, height)
					}
					b[index+1] = 0
				}
			}
		}
	}
}

func TestDirtyBlocks(t *testing.T) {
	const width, height = 129, 131
	previous := make([]byte, width*height*4)
	pixels := append([]byte(nil), previous...)
	all := DirtyBlocks(nil, pixels, width, height, 0)
	if len(all) != 9 || all[8] != (Block{128, 128, 1, 3}) {
		t.Fatalf("edge geometry: %+v", all)
	}
	for i := 3; i < len(pixels); i += 4 {
		pixels[i] = 255
	}
	if len(DirtyBlocks(previous, pixels, width, height, 0)) != 0 {
		t.Fatal("alpha-only update marked blocks dirty")
	}
	for _, point := range [][2]int{{0, 0}, {63, 63}, {64, 64}, {127, 127}, {128, 130}} {
		i := (point[1]*width + point[0]) * 4
		pixels[i], pixels[i+1], pixels[i+2] = 1, 2, 3
		bx, by := point[0]/64*64, point[1]/64*64
		want := []Block{{bx, by, min(64, width-bx), min(64, height-by)}}
		if got := DirtyBlocks(previous, pixels, width, height, 0); !reflect.DeepEqual(got, want) {
			t.Fatalf("point=%v blocks=%+v want=%+v", point, got, want)
		}
		if len(DirtyBlocks(previous, pixels, width, height, 6)) != 0 ||
			!reflect.DeepEqual(DirtyBlocks(previous, pixels, width, height, 5), want) {
			t.Fatal("color tolerance changed")
		}
		pixels[i], pixels[i+1], pixels[i+2] = 0, 0, 0
	}
}

func TestPixelAPIsAndBounds(t *testing.T) {
	if !EqualBGR(nil, nil) || RunBGR(nil) != 0 || len(PackBGR(nil, nil)) != 0 || EqualBGR(nil, make([]byte, 4)) {
		t.Fatal("empty pixel operation changed")
	}
	for _, call := range []func(){
		func() { EqualBGR(make([]byte, 3), nil) },
		func() { RunBGR(make([]byte, 3)) },
		func() { PackBGR(make([]byte, 2), make([]byte, 4)) },
		func() { EqualBGRBlock(nil, nil, 4, 1, 1) },
		func() { EqualBGRBlock(nil, nil, -1, 0, 0) },
		func() { DirtyBlocks(nil, make([]byte, 3), 1, 1, 0) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("invalid buffers/geometry accepted")
				}
			}()
			call()
		}()
	}
	pixels := bytes.Repeat([]byte{1, 2, 3, 55}, 4096)
	other := bytes.Repeat([]byte{1, 2, 3, 99}, 4096)
	if !EqualBGRBlock(pixels, other, 256, 64, 64) || RunBGR(pixels) != 4096 ||
		!bytes.Equal(PackBGR(make([]byte, 4096*3), pixels), bytes.Repeat([]byte{1, 2, 3}, 4096)) {
		t.Fatal("selected public implementation changed color data")
	}
}
