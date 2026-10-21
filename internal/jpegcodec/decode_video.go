package jpegcodec

import (
	"fmt"
	"runtime"
)

// VideoPixels is either GPU-decoded, scaled BT.709 NV12 or system/CPU-decoded
// BGRA at source size. The encoder consumes Format without a BGRA round trip.
type VideoPixels struct {
	Data          []byte
	Width, Height int
	Format        string
}

// VideoDecoder lazily opens compute only for compatible JPEG on Windows/Linux.
// One instance belongs to one serialized capture/stream session. Failed GPU
// initialization or dispatch falls back to the existing decoder for that session.
// Unsupported images fall back individually without disabling a working GPU.
type VideoDecoder struct {
	gpu   *NV12Decoder
	tried bool
}

func (d *VideoDecoder) Decode(data []byte, sw, sh, ow, oh int) (VideoPixels, error) {
	if runtime.GOOS == "windows" || runtime.GOOS == "linux" {
		j, err := parseComputeJPEG(data, ow, oh)
		if err == nil {
			if j.width != sw || j.height != sh {
				return VideoPixels{}, fmt.Errorf("JPEG dimensions mismatch: %dx%d, expected %dx%d", j.width, j.height, sw, sh)
			}
			if !d.tried {
				d.tried = true
				d.gpu, _ = NewNV12Decoder()
			}
			if d.gpu != nil {
				pixels, err := d.gpu.backend.decode(j)
				if err == nil {
					return VideoPixels{Data: pixels, Width: ow, Height: oh, Format: "nv12"}, nil
				}
				d.gpu.Close()
				d.gpu = nil
			}
		}
	}
	pixels, w, h, err := DecodeBGRA(data)
	if err != nil {
		return VideoPixels{}, err
	}
	if w != sw || h != sh {
		return VideoPixels{}, fmt.Errorf("JPEG dimensions mismatch: %dx%d, expected %dx%d", w, h, sw, sh)
	}
	return VideoPixels{Data: pixels, Width: w, Height: h, Format: "bgra"}, nil
}
func (d *VideoDecoder) Close() {
	if d.gpu != nil {
		d.gpu.Close()
		d.gpu = nil
	}
	d.tried = false
}
