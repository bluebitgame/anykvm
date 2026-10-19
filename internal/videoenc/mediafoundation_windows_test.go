//go:build windows && cgo

package videoenc

import (
	"anykvm/internal/framerate"
	"bytes"
	"os"
	"os/exec"
	"testing"
)

// The native software path must work without any FFmpeg DLLs. Hardware is a
// separate opt-in because availability depends on the installed GPU driver.
func TestMediaFoundationH264(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_MF") == "" {
		t.Skip("set ANYKVM_TEST_MF=1 on Windows to test the native system encoder")
	}
	for _, hardware := range []bool{false, true} {
		name := "software"
		if hardware {
			if os.Getenv("ANYKVM_TEST_MF_HARDWARE") == "" {
				continue
			}
			name = "hardware"
		}
		t.Run(name, func(t *testing.T) {
			o := Options{Width: 640, Height: 480, FPS: 30, Rate: framerate.New(30000, 1001), Bitrate: 2000, Codec: "h264"}
			e, err := openMediaFoundation(o, hardware)
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			t.Log(e.Name())
			if _, err := e.Encode([]byte{1}, false); err == nil {
				t.Fatal("accepted an incomplete NV12 frame")
			}
			pixels := make([]byte, o.Width*o.Height*3/2)
			for i := o.Width * o.Height; i < len(pixels); i++ {
				pixels[i] = 128
			}
			var stream []byte
			for frame := 0; frame < 12; frame++ {
				for i := 0; i < o.Width*o.Height; i++ {
					pixels[i] = byte(16 + (i/o.Width+i%o.Width+frame*10)%200)
				}
				force := frame == 0 || frame == 6
				data, err := e.Encode(pixels, force)
				if err != nil {
					t.Fatalf("frame %d: %v", frame, err)
				}
				units, err := mediaFoundationUnits(data, 4)
				if err != nil || len(units) == 0 {
					t.Fatalf("frame %d has no complete Annex-B output: %v", frame, err)
				}
				if force {
					present := map[byte]bool{}
					for _, unit := range units {
						present[unit[0]&31] = true
					}
					if !present[5] || !present[7] || !present[8] {
						t.Fatalf("frame %d lacks IDR/SPS/PPS: %x", frame, data)
					}
				}
				stream = append(stream, data...)
			}
			if ffmpeg := os.Getenv("ANYKVM_TEST_FFMPEG"); ffmpeg != "" {
				cmd := exec.Command(ffmpeg, "-v", "error", "-f", "h264", "-i", "pipe:0", "-pix_fmt", "yuv420p", "-f", "rawvideo", "pipe:1")
				cmd.Stdin = bytes.NewReader(stream)
				decoded, err := cmd.Output()
				if err != nil || len(decoded) != 12*len(pixels) {
					t.Fatalf("round-trip decoded %d bytes: %v", len(decoded), err)
				}
			}
			e.Close()
			e.Close()
			if _, err := e.Encode(pixels, false); err == nil {
				t.Fatal("accepted input after closing the native worker")
			}
		})
	}
}
