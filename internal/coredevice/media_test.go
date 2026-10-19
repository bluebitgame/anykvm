package coredevice

import (
	"anykvm/internal/avmedia"
	"bytes"
	"os"
	"testing"
)

func annexBNALs(data []byte) [][]byte {
	var result [][]byte
	start := -1
	for i := 0; i+3 <= len(data); i++ {
		prefix := 0
		if bytes.Equal(data[i:i+3], []byte{0, 0, 1}) {
			prefix = 3
		} else if i+4 <= len(data) && bytes.Equal(data[i:i+4], []byte{0, 0, 0, 1}) {
			prefix = 4
		}
		if prefix > 0 {
			if start >= 0 {
				result = append(result, data[start:i])
			}
			start = i + prefix
			i += prefix - 1
		}
	}
	if start >= 0 {
		result = append(result, data[start:])
	}
	return result
}
func TestRTPHEVCNativeDecode(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_AVMEDIA") == "" {
		t.Skip("set ANYKVM_TEST_AVMEDIA with native FFmpeg libraries")
	}
	decoder, err := avmedia.NewDecoder("hevc")
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	fixture, err := os.ReadFile("../testdata/passthrough.hevc")
	if err != nil {
		t.Fatal(err)
	}
	assembler := hevcAssembler{waiting: true}
	seq := uint16(1)
	stamp := uint32(1)
	frames := 0
	consume := func(payload []byte, marker bool) {
		t.Helper()
		au, _, _, err := assembler.feed(rtpPacket(seq, stamp, marker, payload))
		seq++
		if err != nil {
			t.Fatal(err)
		}
		if len(au) == 0 {
			return
		}
		frame, err := decoder.Decode(au)
		if err != nil {
			t.Fatal(err)
		}
		if frame == nil {
			return
		}
		defer frame.Release()
		if frame.Width != 64 || frame.Height != 48 || len(frame.Data) != 64*48*4 {
			t.Fatalf("unexpected native picture %dx%d", frame.Width, frame.Height)
		}
		frames++
	}
	for _, nal := range annexBNALs(fixture) {
		if len(nal) < 2 {
			continue
		}
		vcl := (nal[0] >> 1 & 63) < 32
		if len(nal) > 20 {
			typ := nal[0] >> 1 & 63
			start := []byte{nal[0]&129 | 98, nal[1], 128 | typ}
			start = append(start, nal[2:12]...)
			consume(start, false)
			end := []byte{nal[0]&129 | 98, nal[1], 64 | typ}
			end = append(end, nal[12:]...)
			end = append(end, displayTrailer...)
			consume(end, vcl)
		} else {
			consume(nal, vcl)
		}
		if vcl {
			stamp++
		}
	}
	if frames == 0 {
		t.Fatal("no frames decoded from RTP HEVC fixture")
	}
}
