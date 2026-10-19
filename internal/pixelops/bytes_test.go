package pixelops

import (
	"bytes"
	"testing"
)

func TestPixelByteRuns(t *testing.T) {
	for _, impl := range testImplementations() {
		t.Run(impl.name, func(t *testing.T) {
			for _, size := range []int{0, 1, 2, 15, 16, 17, 31, 32, 33, 63, 64, 65, 128, 129, 4096} {
				for offset := 0; offset < 32; offset++ {
					p := bytes.Repeat([]byte{197}, size+offset)[offset:]
					if impl.runByte(p) != size || RunByte(p) != size {
						t.Fatal("full run differs")
					}
					for _, index := range []int{1, 15, 16, 31, 32, 63, size - 1} {
						if index >= 1 && index < size {
							p[index] = 5
							if impl.runByte(p) != index || RunByte(p) != index {
								t.Fatalf("size=%d end=%d", size, index)
							}
							p[index] = 197
						}
					}
				}
			}
		})
	}
}
