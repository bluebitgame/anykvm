package jpegcodec

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
)

// RestartJPEG borrows a baseline 4:2:0 JPEG with one independently coded MCU
// per restart interval (the GPU encoder's format). Parsing, comparison and
// cropping do not entropy-decode; optional NewSolidChecker probes do.
// The caller must keep data immutable for the lifetime of this value.
type RestartJPEG struct {
	Width, Height int
	header        []byte
	sof           int
	segments      [][]byte   // entropy without RST/EOI; nil for metadata-only constants
	solidColors   []uint32   // GPU-only: 0=known non-solid; bit24=solid + low24 RGB
	solidDC       [][3]int16 // exact color descriptors for metadata-only MCUs
}

const gpuSolidFlag = uint32(1 << 24)
const gpuSolidMCUTag = uint32(1 << 31)
const gpuSolidDCHigh = uint32(1 << 30)

// HasRecordedColors distinguishes trusted encoder metadata from externally
// parsed JPEGs, which must still use a coefficient probe.
func (j *RestartJPEG) HasRecordedColors() bool {
	return j != nil && len(j.solidColors) > 0 && len(j.solidColors) == len(j.segments)
}

// RecordedSolidColor combines encoder-provided MCU colors without reading any
// entropy bytes. known=true/non-solid prevents pointless Huffman fallback.
func (j *RestartJPEG) RecordedSolidColor(r image.Rectangle) (rgb uint32, solid, known bool) {
	if !j.HasRecordedColors() || !j.validRect(r) {
		return 0, false, false
	}
	columns, first := (j.Width+15)/16, true
	for y := r.Min.Y / 16; y < (r.Max.Y+15)/16; y++ {
		for x := r.Min.X / 16; x < (r.Max.X+15)/16; x++ {
			value := j.solidColors[y*columns+x]
			if value&gpuSolidFlag == 0 {
				return 0, false, true
			}
			color := value & 0xffffff
			if !first && color != rgb {
				return 0, false, true
			}
			rgb, first = color, false
		}
	}
	return rgb, !first, true
}

// ParseRestartJPEG validates the structure needed to splice MCU payloads.
// Other JPEG layouts should use lossless coefficient-domain cropping instead.
func ParseRestartJPEG(data []byte) (*RestartJPEG, error) {
	invalid := func() (*RestartJPEG, error) {
		return nil, fmt.Errorf("JPEG is not an independent-MCU baseline 4:2:0 scan")
	}
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return invalid()
	}
	j := &RestartJPEG{}
	restart := false
	pos := 2
	for pos+4 <= len(data) {
		if data[pos] != 0xff {
			return invalid()
		}
		marker := data[pos+1]
		n := int(binary.BigEndian.Uint16(data[pos+2:]))
		if n < 2 || n > len(data)-pos-2 {
			return invalid()
		}
		p := data[pos+4 : pos+2+n]
		switch marker {
		case 0xc0:
			if j.Width != 0 || len(p) != 15 || p[0] != 8 || p[5] != 3 ||
				!bytes.Equal(p[6:], []byte{1, 0x22, 0, 2, 0x11, 1, 3, 0x11, 1}) {
				return invalid()
			}
			j.Height, j.Width = int(binary.BigEndian.Uint16(p[1:])), int(binary.BigEndian.Uint16(p[3:]))
			if j.Width < 1 || j.Height < 1 || j.Width > 8192 || j.Height > 8192 {
				return invalid()
			}
			j.sof = pos + 4
		case 0xdd:
			if len(p) != 2 || binary.BigEndian.Uint16(p) != 1 {
				return invalid()
			}
			restart = true
		case 0xda:
			if !restart || j.Width == 0 || !bytes.Equal(p, []byte{3, 1, 0, 2, 0x11, 3, 0x11, 0, 63, 0}) {
				return invalid()
			}
			pos += 2 + n
			j.header = data[:pos]
			start := pos
			count := ((j.Width + 15) / 16) * ((j.Height + 15) / 16)
			for pos < len(data)-1 {
				if data[pos] != 0xff {
					pos++
					continue
				}
				next := data[pos+1]
				if next == 0 {
					pos += 2
					continue
				}
				if pos == start || len(j.segments) >= count {
					return invalid()
				}
				j.segments = append(j.segments, data[start:pos])
				if next == 0xd9 {
					if len(j.segments) != count || pos+2 != len(data) {
						return invalid()
					}
					return j, nil
				}
				if len(j.segments) >= count || next != byte(0xd0+(len(j.segments)-1)%8) {
					return invalid()
				}
				pos += 2
				start = pos
			}
			return invalid()
		case 0xdb, 0xc4: // Preserve quantization and Huffman tables verbatim.
		default:
			// Only our self-contained GPU header is eligible. APP/COM metadata
			// may depend on image dimensions/color interpretation; use TurboJPEG.
			return invalid()
		}
		pos += 2 + n
	}
	return invalid()
}

// SameParameters requires identical tables, dimensions and scan parameters.
func (j *RestartJPEG) SameParameters(other *RestartJPEG) bool {
	return other != nil && bytes.Equal(j.header, other.header)
}

func (j *RestartJPEG) validRect(r image.Rectangle) bool {
	return !r.Empty() && r.Min.X >= 0 && r.Min.Y >= 0 && r.Max.X <= j.Width && r.Max.Y <= j.Height &&
		r.Min.X%16 == 0 && r.Min.Y%16 == 0 && (r.Max.X%16 == 0 || r.Max.X == j.Width) && (r.Max.Y%16 == 0 || r.Max.Y == j.Height)
}

// EqualRect compares MCU entropy or exact constant DC descriptors, without pixels.
func (j *RestartJPEG) EqualRect(other *RestartJPEG, r image.Rectangle) bool {
	if !j.SameParameters(other) || !j.validRect(r) {
		return false
	}
	columns := (j.Width + 15) / 16
	for y := r.Min.Y / 16; y < (r.Max.Y+15)/16; y++ {
		for x := r.Min.X / 16; x < (r.Max.X+15)/16; x++ {
			i := y*columns + x
			if !j.equalMCU(other, i, i) {
				return false
			}
		}
	}
	return true
}

// Crop copies existing entropy, patches dimensions and renumbers restart markers.
// Metadata-only constants synthesize their tiny DC/EOB streams on demand for
// JPEG consumers, retaining exact coefficients without pixels or requantization.
func (j *RestartJPEG) Crop(r image.Rectangle) ([]byte, error) {
	if !j.validRect(r) {
		return nil, fmt.Errorf("JPEG crop is not MCU aligned: %v", r)
	}
	out := append([]byte(nil), j.header...)
	binary.BigEndian.PutUint16(out[j.sof+1:], uint16(r.Dy()))
	binary.BigEndian.PutUint16(out[j.sof+3:], uint16(r.Dx()))
	columns, ordinal := (j.Width+15)/16, 0
	for y := r.Min.Y / 16; y < (r.Max.Y+15)/16; y++ {
		for x := r.Min.X / 16; x < (r.Max.X+15)/16; x++ {
			if ordinal > 0 {
				out = append(out, 0xff, byte(0xd0+(ordinal-1)%8))
			}
			i := y*columns + x
			if len(j.segments[i]) == 0 && j.HasRecordedColors() && j.solidColors[i]&gpuSolidFlag != 0 {
				out = append(out, constantMCUEntropy(j.solidDC[i])...)
			} else {
				out = append(out, j.segments[i]...)
			}
			ordinal++
		}
	}
	return append(out, 0xff, 0xd9), nil
}

// JPEGTile contains a standalone losslessly cropped JPEG and its source region.
type JPEGTile struct {
	Rect image.Rectangle
	Data []byte
}

// EqualMovedRect verifies a copy against the viewer's immutable compressed
// baseline. Complete aligned MCUs with identical tables decode identically.
func (j *RestartJPEG) EqualMovedRect(previous *RestartJPEG, source, destination image.Rectangle) bool {
	if !j.SameParameters(previous) || source.Size() != destination.Size() || !previous.validRect(source) || !j.validRect(destination) ||
		source.Min.X%16 != 0 || source.Min.Y%16 != 0 || source.Max.X%16 != 0 || source.Max.Y%16 != 0 ||
		destination.Min.X%16 != 0 || destination.Min.Y%16 != 0 || destination.Max.X%16 != 0 || destination.Max.Y%16 != 0 {
		return false
	}
	cols := (j.Width + 15) / 16
	for y := 0; y < destination.Dy()/16; y++ {
		for x := 0; x < destination.Dx()/16; x++ {
			a := ((source.Min.Y/16)+y)*cols + source.Min.X/16 + x
			b := ((destination.Min.Y/16)+y)*cols + destination.Min.X/16 + x
			if !j.equalMCU(previous, b, a) {
				return false
			}
		}
	}
	return true
}

func (j *RestartJPEG) equalMCU(other *RestartJPEG, a, b int) bool {
	if len(j.segments[a]) != 0 || len(other.segments[b]) != 0 {
		return bytes.Equal(j.segments[a], other.segments[b])
	}
	return j.HasRecordedColors() && other.HasRecordedColors() &&
		j.solidColors[a]&gpuSolidFlag != 0 && other.solidColors[b]&gpuSolidFlag != 0 &&
		j.solidDC[a] == other.solidDC[b]
}
