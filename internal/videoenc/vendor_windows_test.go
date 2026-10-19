//go:build windows && cgo && !ffmpeg

package videoenc

import (
	"anykvm/internal/av1"
	"anykvm/internal/framerate"
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// This intentionally opens the vendor backend directly, bypassing both MF and
// libavcodec. Run on the selected GPU host with no FFmpeg DLLs installed.
func TestWindowsVendorNative(t *testing.T) {
	vendors := os.Getenv("ANYKVM_TEST_WINDOWS_VENDOR")
	if vendors == "" {
		t.Skip("set ANYKVM_TEST_WINDOWS_VENDOR=nvenc,amf,qsv (choose installed GPUs) on Windows")
	}
	codecs := []string{"h264"}
	if os.Getenv("ANYKVM_TEST_VENDOR_AV1") != "" {
		codecs = append(codecs, "av1")
	}
	for _, vendor := range strings.Split(vendors, ",") {
		vendor = strings.TrimSpace(vendor)
		if vendor != "nvenc" && vendor != "amf" && vendor != "qsv" {
			t.Fatalf("unknown native vendor %q", vendor)
		}
		for _, codec := range codecs {
			t.Run(vendor+"/"+codec, func(t *testing.T) {
				// Exercise non-aligned dimensions and exact rational frame rates.
				o := Options{Width: 638, Height: 478, FPS: 30, Rate: framerate.New(30000, 1001), Bitrate: 2000, Codec: codec}
				e, err := openWindowsVendor(o, codec+"_"+vendor)
				if err != nil {
					t.Fatal(err)
				}
				defer e.Close()
				t.Log(e.Name())
				bad := o
				ids := map[string]uint32{"nvenc": 0x10de, "amf": 0x1002, "qsv": 0x8086}
				bad.WindowsAdapter = WindowsAdapter{Set: true, VendorID: ids[vendor], LUID: ^uint64(0)}
				if other, err := openWindowsVendor(bad, codec+"_"+vendor); err == nil {
					other.Close()
					t.Fatal("native opener silently substituted a GPU for a nonexistent LUID")
				}
				if _, err := e.Encode([]byte{1}, false); err == nil {
					t.Fatal("accepted incomplete native NV12 input")
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
					data, err := e.Encode(pixels, frame == 6)
					if err != nil {
						t.Fatalf("frame %d: %v", frame, err)
					}
					if len(data) == 0 {
						t.Fatalf("frame %d buffered despite low-delay settings", frame)
					}
					if codec == "av1" {
						if _, err := av1.Inspect(data); err != nil {
							t.Fatalf("frame %d has invalid AV1 output: %v", frame, err)
						}
					} else if _, err := mediaFoundationUnits(data, 4); err != nil {
						t.Fatalf("frame %d has invalid Annex-B output: %v", frame, err)
					}
					if (frame == 0 || frame == 6) && !mediaFoundationRecovery(data, codec) {
						t.Fatalf("frame %d has no native recovery picture", frame)
					}
					stream = append(stream, data...)
				}
				if ffmpeg := os.Getenv("ANYKVM_TEST_FFMPEG"); ffmpeg != "" {
					format := codec
					if codec == "av1" {
						format = "obu"
					}
					cmd := exec.Command(ffmpeg, "-v", "error", "-f", format, "-i", "pipe:0", "-pix_fmt", "yuv420p", "-f", "rawvideo", "pipe:1")
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
}
