//go:build darwin && cgo

package audiodec

import (
	"anykvm/internal/capture"
	"encoding/binary"
	"os"
	"testing"
)

func TestNativeAACELDDecode(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_SYSTEM_AUDIO") == "" {
		t.Skip("set ANYKVM_TEST_SYSTEM_AUDIO to enable system codec checks")
	}
	f := capture.AudioFrame{Codec: "mp4a.40.39", SampleRate: 48000, Channels: 2, Config: []byte{0xf8, 0xe6, 0x50, 0}, FramesPerPacket: 480}
	d, err := New(f)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	data, err := os.ReadFile("../testdata/tone48.aaceld")
	if err != nil {
		t.Fatal(err)
	}
	frames, nonzero := 0, 0
	var previous [2]int16
	var crossings [2]int
	for len(data) >= 2 {
		n := int(binary.BigEndian.Uint16(data))
		data = data[2:]
		if n > len(data) {
			t.Fatal("truncated fixture")
		}
		pcm, err := d.Decode(data[:n])
		data = data[n:]
		if err != nil {
			t.Fatal(err)
		}
		if len(pcm)%4 != 0 {
			t.Fatal("unaligned PCM")
		}
		frames += len(pcm) / 4
		for _, v := range pcm {
			if v != 0 {
				nonzero++
			}
		}
		for i := 0; i+3 < len(pcm); i += 4 {
			for c := 0; c < 2; c++ {
				s := int16(binary.LittleEndian.Uint16(pcm[i+c*2:]))
				if previous[c] <= 0 && s > 0 {
					crossings[c]++
				}
				previous[c] = s
			}
		}
	}
	if frames < 23000 || frames > 25000 || nonzero < 40000 || crossings[0] < 200 || crossings[0] > 240 || crossings[1] < 300 || crossings[1] > 360 {
		t.Fatalf("decoded audio corrupted: frames=%d nonzero=%d tone_cycles=%v", frames, nonzero, crossings)
	}
	t.Logf("decoded %d stereo frames; tone cycles %v", frames, crossings)
}
