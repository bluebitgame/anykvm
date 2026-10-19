//go:build cgo

package native

import (
	"bytes"
	"testing"
)

func TestStridedFECMatchesContiguousAndPreservesGaps(t *testing.T) {
	for _, tc := range []struct {
		data, parity, size int
		audio              bool
	}{
		{1, 2, 528, false}, {44, 9, 1040, false}, {212, 43, 1424, false}, {4, 2, 80, true},
	} {
		n := tc.data + tc.parity
		plain := make([]byte, n*tc.size)
		for i := 0; i < tc.data*tc.size; i++ {
			plain[i] = byte(i*37 + 11)
		}
		stride, offset := tc.size+48, 32
		padded := bytes.Repeat([]byte{0xa5}, n*stride)
		for i := 0; i < tc.data; i++ {
			copy(padded[i*stride+offset:], plain[i*tc.size:(i+1)*tc.size])
		}
		if err := Code(plain, tc.data, tc.parity, tc.size, tc.audio, nil); err != nil {
			t.Fatal(err)
		}
		if err := CodeStrided(padded, tc.data, tc.parity, tc.size, stride, offset, tc.audio, nil); err != nil {
			t.Fatal(err)
		}
		check := func() {
			for i := 0; i < n; i++ {
				if !bytes.Equal(padded[i*stride+offset:i*stride+offset+tc.size], plain[i*tc.size:(i+1)*tc.size]) {
					t.Fatalf("FEC mismatch shard=%d size=%d", i, tc.size)
				}
				for _, gap := range [][]byte{padded[i*stride : i*stride+offset], padded[i*stride+offset+tc.size : (i+1)*stride]} {
					for _, b := range gap {
						if b != 0xa5 {
							t.Fatal("FEC overwrote wire header/tag space")
						}
					}
				}
			}
		}
		check()
		missing := make([]byte, n)
		missing[0] = 1
		clear(padded[offset : offset+tc.size])
		if err := CodeStrided(padded, tc.data, tc.parity, tc.size, stride, offset, tc.audio, missing); err != nil {
			t.Fatal(err)
		}
		check()
	}
}

func TestStridedFECRejectsInvalidLayout(t *testing.T) {
	for _, tc := range []struct{ size, stride, offset, length int }{{0, 0, 0, 0}, {8, 7, 0, 21}, {8, 16, -1, 48}, {8, 16, 9, 48}, {8, 16, 0, 47}, {8, 2147483648, 0, 1}} {
		if err := CodeStrided(make([]byte, tc.length), 1, 2, tc.size, tc.stride, tc.offset, false, nil); err == nil {
			t.Fatal("invalid FEC layout accepted", tc)
		}
	}
}
