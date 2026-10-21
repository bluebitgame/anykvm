package videoenc

import (
	"anykvm/internal/av1"
	"bytes"
	"os"
	"os/exec"
	"testing"
)

// Opt in on an AV1-capable GPU host. Without the ffmpeg tag, Linux uses Vulkan
// Video and Windows uses vendor APIs or Media Foundation.
func TestAV1HardwareRecovery(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_AV1") == "" {
		t.Skip("set ANYKVM_TEST_AV1=1 on an AV1 GPU host")
	}
	o := Options{Width: 1280, Height: 768, FPS: 30, Bitrate: 4000, Codec: "av1"}
	e, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	t.Log("encoder:", e.Name())
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
			t.Fatal(err)
		}
		if len(data) == 0 {
			t.Fatalf("frame %d buffered despite low-delay configuration", frame)
		}
		info, err := av1.Inspect(data)
		if err != nil {
			t.Fatal(err)
		}
		if force && !info.Recovery {
			t.Fatalf("frame %d lacks a recovery frame and sequence header", frame)
		}
		stream = append(stream, data...)
	}
	if ff := os.Getenv("ANYKVM_TEST_FFMPEG"); ff != "" {
		cmd := exec.Command(ff, "-v", "error", "-f", "obu", "-i", "pipe:0", "-pix_fmt", "yuv420p", "-f", "rawvideo", "pipe:1")
		cmd.Stdin = bytes.NewReader(stream)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 12*len(pixels) {
			t.Fatalf("decoded %d bytes, expected %d", len(out), 12*len(pixels))
		}
	}
}
