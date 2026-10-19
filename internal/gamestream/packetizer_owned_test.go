package gamestream

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"testing"
)

func TestPooledPacketBatchesRemainIndependent(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		for _, size := range []int{512, 1024, 1408} {
			a, b := &videoPacketizer{}, &videoPacketizer{}
			if encrypted {
				block, _ := aes.NewCipher(make([]byte, 16))
				a.gcm, _ = cipher.NewGCM(block)
				b.gcm, _ = cipher.NewGCM(block)
			}
			for _, length := range []int{1, 7, 8, 495, 496, 497, 100000, 400000, 13} {
				data := bytes.Repeat([]byte{byte(length)}, length)
				expected, err := a.pack(data, true, 123, size, 2)
				if err != nil {
					t.Fatal(err)
				}
				batch, err := b.packBatch(data, true, 123, size, 2)
				if err != nil {
					t.Fatal(err)
				}
				if len(expected) != len(batch.packets) {
					t.Fatal("packet count changed")
				}
				for i := range expected {
					if !bytes.Equal(expected[i], batch.packets[i]) {
						t.Fatalf("wire mismatch encrypted=%v size=%d len=%d packet=%d", encrypted, size, length, i)
					}
				}
				// A second outstanding batch must never overwrite a still-live batch.
				other, err := b.packBatch([]byte{1}, false, 999, size, 2)
				if err != nil {
					t.Fatal(err)
				}
				for i := range expected {
					if !bytes.Equal(expected[i], batch.packets[i]) {
						t.Fatal("live packet batch overwritten")
					}
				}
				b.sequence = a.sequence
				b.frame = a.frame
				b.iv = a.iv
				other.Release()
				batch.Release()
				batch.Release()
			}
		}
	}
}

func BenchmarkVideoPacketBuffers(b *testing.B) {
	for _, encrypted := range []bool{false, true} {
		for _, pooled := range []bool{false, true} {
			name := map[bool]string{false: "plain", true: "AES-GCM"}[encrypted] + "/" + map[bool]string{false: "allocate", true: "reuse"}[pooled]
			b.Run(name, func(b *testing.B) {
				v := &videoPacketizer{}
				if encrypted {
					block, _ := aes.NewCipher(make([]byte, 16))
					v.gcm, _ = cipher.NewGCM(block)
				}
				data := make([]byte, 41667) // Average access unit at 20 Mbps, 60 fps.
				b.ReportAllocs()
				b.SetBytes(int64(len(data)))
				b.ResetTimer()
				for range b.N {
					if pooled {
						batch, err := v.packBatch(data, false, 0, 1024, 2)
						if err != nil {
							b.Fatal(err)
						}
						batch.Release()
					} else {
						if _, err := v.pack(data, false, 0, 1024, 2); err != nil {
							b.Fatal(err)
						}
					}
				}
			})
		}
	}
}
