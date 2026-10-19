//go:build linux && cgo

package videoenc

import (
	"anykvm/internal/framerate"
	"bytes"
	"os"
	"os/exec"
	"testing"
	"time"
)

// Hardware opt-in: native Vulkan Video, including non-macroblock-aligned
// cropping, reference pictures, forced IDR and an optional decoder round-trip.
func TestNativeVulkanVideo(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_VULKAN") == "" {
		t.Skip("set ANYKVM_TEST_VULKAN=1 on the Linux Vulkan Video GPU host")
	}
	o := Options{Width: 638, Height: 478, FPS: 30, Rate: framerate.New(30000, 1001), Bitrate: 4000, Codec: "h264"}
	e, err := openLinuxVulkan(o)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	pixels := make([]byte, o.Width*o.Height*3/2)
	for i := o.Width * o.Height; i < len(pixels); i++ {
		pixels[i] = 128
	}
	var stream []byte
	for n := 0; n < 12; n++ {
		for i := 0; i < o.Width*o.Height; i++ {
			pixels[i] = byte(16 + (i/o.Width+i%o.Width+n*10)%200)
		}
		start := time.Now()
		data, err := e.Encode(pixels, n == 6)
		if err != nil {
			t.Fatalf("frame %d: %v", n, err)
		}
		if len(data) == 0 {
			t.Fatal("native encoder buffered the picture")
		}
		if n == 0 || n == 6 {
			if !vulkanNAL(data, 7) || !vulkanNAL(data, 8) || !vulkanNAL(data, 5) {
				t.Fatalf("frame %d lacks SPS/PPS/IDR", n)
			}
		}
		stream = append(stream, data...)
		t.Logf("frame %d CPU upload + encode: %s", n, time.Since(start))
	}
	if executable := os.Getenv("ANYKVM_TEST_FFMPEG"); executable != "" {
		command := exec.Command(executable, "-hide_banner", "-loglevel", "error", "-f", "h264", "-i", "pipe:0", "-f", "rawvideo", "-pix_fmt", "gray", "pipe:1")
		command.Stdin = bytes.NewReader(stream)
		var diagnostics bytes.Buffer
		command.Stderr = &diagnostics
		decoded, err := command.Output()
		if err != nil {
			t.Fatalf("decode: %v %s", err, diagnostics.String())
		}
		if len(decoded) != o.Width*o.Height*12 {
			t.Fatalf("decoded size %d; crop/frame count is wrong", len(decoded))
		}
	}
}
func vulkanNAL(data []byte, kind byte) bool {
	for i := 0; i+3 < len(data); i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 && data[i+3]&31 == kind {
			return true
		}
	}
	return false
}
