//go:build linux && cgo

package videoenc

import (
	"anykvm/internal/framerate"
	"bytes"
	"os"
	"os/exec"
	"testing"
)

// Opt in on a Raspberry Pi with the bcm2835-codec encoder driver. No FFmpeg
// library is needed; an optional executable checks decoded frame dimensions.
func TestRaspberryPiNativeH264(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_RPI_H264") == "" {
		t.Skip("set ANYKVM_TEST_RPI_H264=1 on a Raspberry Pi with H.264 hardware encoding")
	}
	for _, dimensions := range [][2]int{{640, 480}, {638, 478}} {
		o := Options{Width: dimensions[0], Height: dimensions[1], Rate: framerate.New(30000, 1001), FPS: 30, Bitrate: 2000, Codec: "h264"}
		e, err := openRaspberryPi(o)
		if err != nil {
			t.Fatal(err)
		}
		defer e.Close()
		if e.Name() != "h264_v4l2m2m_native" {
			t.Fatalf("selected %s", e.Name())
		}
		if _, err := e.Encode([]byte{1}, false); err == nil {
			t.Fatal("accepted incomplete input")
		}
		pixels := make([]byte, o.Width*o.Height*3/2)
		for i := o.Width * o.Height; i < len(pixels); i++ {
			pixels[i] = 128
		}
		var stream []byte
		inter := false
		for frame := 0; frame < 12; frame++ {
			for i := 0; i < o.Width*o.Height; i++ {
				pixels[i] = byte(16 + (i/o.Width+i%o.Width+frame*10)%200)
			}
			data, err := e.Encode(pixels, frame == 6)
			if err != nil {
				t.Fatalf("frame %d: %v", frame, err)
			}
			if len(data) == 0 {
				t.Fatalf("frame %d was buffered", frame)
			}
			units, err := mediaFoundationUnits(data, 4)
			if err != nil {
				t.Fatalf("frame %d has invalid Annex-B output: %v", frame, err)
			}
			idr := false
			for _, unit := range units {
				idr = idr || unit[0]&31 == 5
			}
			if (frame == 0 || frame == 6) && !idr {
				t.Fatalf("frame %d has no recovery picture", frame)
			}
			inter = inter || !idr
			stream = append(stream, data...)
		}
		if !inter {
			t.Fatal("encoder produced only IDR pictures")
		}
		if ffmpeg := os.Getenv("ANYKVM_TEST_FFMPEG"); ffmpeg != "" {
			cmd := exec.Command(ffmpeg, "-v", "error", "-f", "h264", "-i", "pipe:0", "-pix_fmt", "yuv420p", "-f", "rawvideo", "pipe:1")
			cmd.Stdin = bytes.NewReader(stream)
			decoded, err := cmd.Output()
			if err != nil || len(decoded) != 12*len(pixels) {
				t.Fatalf("decoded %d bytes: %v", len(decoded), err)
			}
		}
		e.Close()
		e.Close()
		if _, err := e.Encode(pixels, false); err == nil {
			t.Fatal("accepted input after closing")
		}
	}
}

func TestRaspberryPiRejectsNonCodecDevice(t *testing.T) {
	o := Options{Width: 640, Height: 480, FPS: 30, Bitrate: 2000, Codec: "h264"}
	if e, err := openRaspberryPiDevice(o, "/dev/null"); err == nil {
		e.Close()
		t.Fatal("selected a non-codec device")
	}
}
