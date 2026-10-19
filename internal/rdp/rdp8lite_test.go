package rdp

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func liteTestPacket(bits string) []byte {
	bits = strings.ReplaceAll(bits, " ", "")
	p := make([]byte, (len(bits)+7)/8)
	for i, bit := range bits {
		if bit == '1' {
			p[i/8] |= 1 << uint(7-i%8)
		}
	}
	return append(append([]byte{0xe0, 0x26}, p...), byte((8-len(bits)%8)%8))
}

func liteTestRun(p []byte) []byte {
	bits := "1000100000" + fmt.Sprintf("%015b", len(p))
	bits += strings.Repeat("0", (8-len(bits)%8)%8)
	encoded := liteTestPacket(bits)
	return append(append(encoded[:len(encoded)-1], p...), 0)
}

func liteTestLength(n int) string {
	if n == 3 {
		return "0"
	}
	width := 2
	for 1<<(width+1) <= n {
		width++
	}
	return strings.Repeat("1", width-1) + "0" + fmt.Sprintf("%0*b", width, n-(1<<width))
}

func TestRDP8LiteSpecificationExamples(t *testing.T) {
	for _, tc := range []struct {
		bits string
		want []byte
	}{
		{"0 01001001", []byte{0x49}},
		{"0 01001001 10001 00001 110 001", bytes.Repeat([]byte{0x49}, 10)},
		{"11000 11001 110100 110101 110110 11111111", []byte{0, 1, 2, 3, 255, 0x66}},
	} {
		var d rdp8LiteDecoder
		out, err := d.decode(liteTestPacket(tc.bits))
		if err != nil || !bytes.Equal(out, tc.want) {
			t.Fatalf("specification vector %q: output=%x error=%v", tc.bits, out, err)
		}
	}
	var d rdp8LiteDecoder
	plain := []byte("aligned raw run followed by a token")
	p := liteTestRun(plain)
	// The raw run is byte-aligned; a normal literal follows before the trailer.
	tail := liteTestPacket("0 01001001")
	p = append(p[:len(p)-1], tail[2:]...)
	out, err := d.decode(p)
	if err != nil || !bytes.Equal(out, append(plain, 0x49)) {
		t.Fatalf("raw run and continuation: output=%x error=%v", out, err)
	}
}

func TestRDP8LiteHistoryAndLengthBoundaries(t *testing.T) {
	for _, n := range []int{3, 4, 7, 8, 15, 16, 31, 32, 63, 64, 127, 128, 255, 256, 511, 512, 1023, 1024, 2047, 2048, 4095, 4096, 8191, 8192} {
		var d rdp8LiteDecoder
		if _, err := d.decode([]byte{0xe0, 6, 0x49}); err != nil {
			t.Fatal(err)
		}
		out, err := d.decode(liteTestPacket("10001 00001 " + liteTestLength(n)))
		if err != nil || !bytes.Equal(out, bytes.Repeat([]byte{0x49}, n)) {
			t.Fatalf("match length %d: output length=%d error=%v", n, len(out), err)
		}
	}
	var d rdp8LiteDecoder
	var history []byte
	for i := 0; i < 7; i++ {
		p := bytes.Repeat([]byte{byte(i + 1)}, 1500)
		if _, err := d.decode(append([]byte{0xe0, 6}, p...)); err != nil {
			t.Fatal(err)
		}
		history = append(history, p...)
	}
	want := history[len(history)-liteWindow : len(history)-liteWindow+3]
	out, err := d.decode(liteTestPacket("101100 " + fmt.Sprintf("%014b", liteWindow-5792) + " 0"))
	if err != nil || !bytes.Equal(out, want) {
		t.Fatalf("maximum match distance across wrapped history: output=%x expected=%x error=%v", out, want, err)
	}
}

func TestRDP8LiteInvalidDataAndHistoryCommit(t *testing.T) {
	var d rdp8LiteDecoder
	if _, err := d.decode([]byte{0xe0, 6, 0x49}); err != nil {
		t.Fatal(err)
	}
	before := d
	for _, p := range [][]byte{
		nil, {0xe0}, {0xe2, 6}, {0xe0, 4, 1}, {0xe0, 0x26},
		{0xe0, 0x26, 8}, {0xe0, 0x26, 7},
		liteTestPacket("0 00000000"), // Reserved long encoding for a short literal.
		liteTestPacket("10000 0"),
		liteTestPacket("0 01001001 10001"),
		liteTestPacket("10001 00001 " + liteTestLength(8193)),
		liteTestPacket("101100 " + fmt.Sprintf("%014b", 8193-5792) + " 0"),
		liteTestPacket("10001 00000 111111111111111"),
		join([]byte{0xe1}, u16(2), u32(0), u32(1), []byte{6}),
		join([]byte{0xe1}, u16(1), u32(2), u32(2), []byte{6, 0x49}),
		bytes.Repeat([]byte{0xe0}, vcChunk+1),
	} {
		if _, err := d.decode(p); err == nil {
			t.Fatalf("accepted invalid RDP8 Lite segment: %x", p)
		}
		if d != before {
			t.Fatal("invalid segment changed the history")
		}
	}
	out, err := d.decode(join([]byte{0xe1}, u16(1), u32(1), u32(2), []byte{6, 0x49}))
	if err != nil || !bytes.Equal(out, []byte{0x49}) {
		t.Fatalf("single-segment multipart container: output=%x error=%v", out, err)
	}
}

func TestDVCVersion3MixedCompressedFragments(t *testing.T) {
	p := gfxPDU(0xd, join(u32(0), u32(2), u32(2)))
	for _, compressedFirst := range []bool{false, true} {
		c := channelTestClient()
		c.dvcClientVersion = 3
		c.dvcVersion = 3
		first := join([]byte{0x20, 1, byte(len(p))}, p[:5])
		if compressedFirst {
			first = join([]byte{0x60, 1, byte(len(p))}, liteTestRun(p[:5]))
		}
		for i, packet := range [][]byte{
			first,
			join([]byte{0x30, 1}, p[5:12]),
			join([]byte{0x70, 1}, liteTestRun(p[12:])),
		} {
			if err := c.handleDVC(packet); err != nil {
				t.Fatalf("compressed first=%t fragment %d: %v", compressedFirst, i, err)
			}
			if i < 2 && c.acknowledged != 0 {
				t.Fatal("processed an incomplete graphics acknowledgment")
			}
		}
		if c.dvcTotal != 0 || c.acknowledged != 2 {
			t.Fatalf("mixed compressed message not reconstructed: total=%d acknowledged=%d", c.dvcTotal, c.acknowledged)
		}
	}
	for _, version := range []uint16{0, 1, 2} {
		c := channelTestClient()
		c.dvcClientVersion = version
		c.dvcVersion = version
		for _, packet := range [][]byte{
			join([]byte{0x60, 1, byte(len(p))}, liteTestRun(p)),
			join([]byte{0x70, 1}, liteTestRun(p)),
		} {
			if err := c.handleDVC(packet); err == nil {
				t.Fatalf("accepted compressed data without version 3 negotiation: version=%d packet=%x", version, packet)
			}
		}
	}
}

func FuzzRDP8LiteBoundedDecode(f *testing.F) {
	f.Add(liteTestPacket("0 01001001 10001 00001 110 001"))
	f.Add(liteTestRun([]byte("graphics capabilities")))
	f.Add([]byte{0xe0, 6, 0x49})
	f.Fuzz(func(t *testing.T, p []byte) {
		var d rdp8LiteDecoder
		if _, err := d.decode([]byte{0xe0, 6, 0x49}); err != nil {
			t.Fatal(err)
		}
		before := d
		out, err := d.decode(p)
		if len(out) > liteWindow || (err != nil && d != before) {
			t.Fatal("decoder exceeded its bounds or committed invalid history")
		}
	})
}
