//go:build linux && cgo

package videoenc

import (
	"anykvm/internal/av1"
	"anykvm/internal/framerate"
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Hardware opt-in: native Vulkan Video, including non-macroblock-aligned
// cropping, reference pictures, forced IDR and an optional decoder round-trip.
func TestNativeVulkanVideo(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_VULKAN") == "" {
		t.Skip("set ANYKVM_TEST_VULKAN=1 on the Linux Vulkan Video GPU host")
	}
	codecs := os.Getenv("ANYKVM_TEST_VULKAN_CODECS")
	if codecs == "" {
		codecs = "h264"
	}
	for _, codec := range strings.Split(codecs, ",") {
		codec = strings.TrimSpace(codec)
		if codec != "h264" && codec != "hevc" && codec != "av1" {
			t.Fatalf("unknown Vulkan codec %q", codec)
		}
		t.Run(codec, func(t *testing.T) { testNativeVulkanVideo(t, codec) })
	}
}

func testNativeVulkanVideo(t *testing.T, codec string) {
	o := Options{Width: 638, Height: 478, FPS: 30, Rate: framerate.New(30000, 1001), Bitrate: 4000, Codec: codec}
	if codec == "av1" {
		// Keep AV1's extent aligned for drivers that code larger blocks.
		o.Width, o.Height = 768, 512
	}
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
			if codec == "av1" {
				info, err := av1.Inspect(data)
				if err != nil || !info.Recovery {
					t.Fatalf("frame %d lacks AV1 sequence/key picture: %+v %v", n, info, err)
				}
			} else if codec == "hevc" {
				if !vulkanHEVCNAL(data, 32) || !vulkanHEVCNAL(data, 33) || !vulkanHEVCNAL(data, 34) || (!vulkanHEVCNAL(data, 19) && !vulkanHEVCNAL(data, 20)) {
					t.Fatalf("frame %d lacks HEVC VPS/SPS/PPS/IDR", n)
				}
			} else if !vulkanNAL(data, 7) || !vulkanNAL(data, 8) || !vulkanNAL(data, 5) {
				t.Fatalf("frame %d lacks SPS/PPS/IDR", n)
			}
		}
		stream = append(stream, data...)
		t.Logf("frame %d CPU upload + encode: %s", n, time.Since(start))
	}
	if executable := os.Getenv("ANYKVM_TEST_FFMPEG"); executable != "" {
		format := codec
		if format == "av1" {
			format = "obu"
		}
		command := exec.Command(executable, "-hide_banner", "-loglevel", "error", "-f", format, "-i", "pipe:0", "-f", "rawvideo", "-pix_fmt", "gray", "pipe:1")
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
		first, last := int(decoded[0]), int(decoded[len(decoded)-o.Width*o.Height])
		if last-first < 40 {
			t.Fatalf("frame updates lost: first=%d last=%d", first, last)
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

func vulkanHEVCNAL(data []byte, kind byte) bool {
	for i := 0; i+4 < len(data); i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 && (data[i+3]>>1)&63 == kind {
			return true
		}
	}
	return false
}
