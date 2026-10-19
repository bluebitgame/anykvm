//go:build darwin && cgo

package capture

// #include <stdlib.h>
// extern char *avfoundation_modes(const char *deviceName);
import "C"

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unsafe"
)

var nativePixelFormats = map[uint32]string{
	0x34323076: "nv12", 0x34323066: "nv12_full", 0x79757673: "yuyv422", 0x32767579: "uyvy422",
	0x42475241: "bgra", 0x6a706567: "mjpeg", 0x646d6231: "mjpeg", 0x6d6a7067: "mjpeg", 0x61766331: "h264",
}

func nativeFormatName(code uint32) string {
	if name, ok := nativePixelFormats[code]; ok {
		return name
	}
	return fmt.Sprintf("fourcc_%08x", code)
}
func nativeFormatCode(name string) uint32 {
	// Prefer the canonical JPEG subtype if multiple names map to MJPEG.
	if name == "mjpeg" {
		return 0x6a706567
	}
	for code, value := range nativePixelFormats {
		if value == name {
			return code
		}
	}
	if strings.HasPrefix(name, "fourcc_") {
		code, _ := strconv.ParseUint(strings.TrimPrefix(name, "fourcc_"), 16, 32)
		return uint32(code)
	}
	return 0
}

// ListVideoModes queries device metadata without opening a capture session.
func ListVideoModes(device string) ([]VideoMode, error) {
	if strings.TrimSpace(device) == "" || IsScreenDevice(device) || strings.Contains(device, ":") {
		return nil, fmt.Errorf("capture modes are available for local capture devices")
	}
	name := C.CString(device)
	defer C.free(unsafe.Pointer(name))
	data := C.avfoundation_modes(name)
	if data == nil {
		return nil, fmt.Errorf("capture device %q was not found or has no modes", device)
	}
	defer C.free(unsafe.Pointer(data))
	var native []struct {
		Width          int
		Height         int
		Subtype        uint32
		MinFPS, MaxFPS float64
	}
	if err := json.Unmarshal([]byte(C.GoString(data)), &native); err != nil {
		return nil, err
	}
	modes := make([]VideoMode, 0, len(native))
	seen := map[VideoMode]bool{}
	for _, value := range native {
		mode := VideoMode{Width: value.Width, Height: value.Height, PixelFormat: nativeFormatName(value.Subtype), MinFPS: value.MinFPS, MaxFPS: value.MaxFPS}
		if mode.Width <= 0 || mode.Height <= 0 || mode.MinFPS <= 0 || mode.MaxFPS < mode.MinFPS {
			continue
		}
		if !seen[mode] {
			modes = append(modes, mode)
			seen[mode] = true
		}
	}
	sort.Slice(modes, func(i, j int) bool {
		a, b := modes[i], modes[j]
		if a.Width != b.Width {
			return a.Width > b.Width
		}
		if a.Height != b.Height {
			return a.Height > b.Height
		}
		if a.PixelFormat != b.PixelFormat {
			return a.PixelFormat < b.PixelFormat
		}
		if a.MaxFPS != b.MaxFPS {
			return a.MaxFPS > b.MaxFPS
		}
		return a.MinFPS < b.MinFPS
	})
	return modes, nil
}

func chooseVideoMode(modes []VideoMode, request Mode) (VideoMode, error) {
	var best VideoMode
	bestScore := -1
	for _, mode := range modes {
		if request.Width > 0 && (mode.Width != request.Width || mode.Height != request.Height) {
			continue
		}
		if request.PixelFormat != "" && mode.PixelFormat != request.PixelFormat {
			continue
		}
		if request.FPS > 0 && (request.FPS < mode.MinFPS-0.001 || request.FPS > mode.MaxFPS+0.001) {
			continue
		}
		score := mode.Width * mode.Height / 100
		if mode.PixelFormat == "mjpeg" {
			score += 100000000
		}
		if score > bestScore {
			best, bestScore = mode, score
		}
	}
	if bestScore < 0 {
		return VideoMode{}, fmt.Errorf("unsupported capture mode %dx%d@%g:%s", request.Width, request.Height, request.FPS, request.PixelFormat)
	}
	return best, nil
}

func (d *darwinDevice) SetCaptureMode(value string) error {
	mode, err := ParseMode(value)
	if err != nil {
		return err
	}
	d.mode = mode
	return nil
}

func (d *darwinDevice) probeMode() (deviceCaps, error) {
	modes, err := ListVideoModes(d.id)
	if err != nil {
		return deviceCaps{}, err
	}
	chosen, err := chooseVideoMode(modes, d.mode)
	if err != nil {
		return deviceCaps{}, err
	}
	d.sourceFPS = chosen.MaxFPS
	if d.mode.FPS > 0 {
		d.sourceFPS = d.mode.FPS
	}
	// Preserve the selected subtype, including JPEG variants exposed by drivers.
	d.sourceFormat = nativeFormatCode(chosen.PixelFormat)
	return deviceCaps{width: chosen.Width, height: chosen.Height, fps: int(math.Round(d.sourceFPS)), mjpeg: chosen.PixelFormat == "mjpeg", h264: chosen.PixelFormat == "h264", ok: true}, nil
}
