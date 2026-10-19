// Package av1 inspects low-overhead AV1 OBUs emitted by the hardware encoders.
package av1

import (
	"errors"
	"fmt"
)

var errInvalid = errors.New("invalid AV1 temporal unit")

type Info struct {
	Recovery bool   // sequence header and a shown key frame with picture data
	Codec    string // WebCodecs string for the supported Main, 8-bit output
}

// Inspect accepts size-delimited low-overhead OBUs, not IVF or Annex B.
// Only the sequence/frame header prefix needed for recovery is parsed.
func Inspect(data []byte) (Info, error) {
	var info Info
	var sequence, reduced, key, picture bool
	if len(data) == 0 {
		return info, errInvalid
	}
	for len(data) > 0 {
		header := data[0]
		data = data[1:]
		if header&0x81 != 0 || header&2 == 0 {
			return info, errInvalid
		}
		kind := (header >> 3) & 15
		if header&4 != 0 {
			if len(data) == 0 || data[0]&7 != 0 {
				return info, errInvalid
			}
			data = data[1:]
		}
		var size uint64
		finished := false
		for i := 0; i < 8; i++ {
			if len(data) == 0 {
				return info, errInvalid
			}
			b := data[0]
			data = data[1:]
			size |= uint64(b&127) << uint(7*i)
			if b&128 == 0 {
				finished = true
				break
			}
		}
		if !finished || size > uint64(len(data)) {
			return info, errInvalid
		}
		payload := data[:int(size)]
		data = data[int(size):]
		switch kind {
		case 1:
			// The encoder uses NV12 input and AV1 Main profile (8-bit SDR).
			b := bits{data: payload}
			if b.read(3) != 0 {
				return info, errInvalid
			}
			still := b.read(1)
			reduced = b.read(1) != 0
			if reduced && still == 0 {
				return info, errInvalid
			}
			var level, tier uint32
			if reduced {
				level = b.read(5)
			} else {
				if b.read(1) != 0 { // timing_info_present_flag
					b.read(32)
					b.read(32)
					if b.read(1) != 0 { // equal_picture_interval, followed by uvlc
						n := 0
						for n < 32 && b.read(1) == 0 && !b.bad {
							n++
						}
						if n < 32 {
							b.read(n)
						}
					}
					if b.read(1) != 0 {
						b.read(5)
						b.read(32)
						b.read(5)
						b.read(5)
					}
				}
				b.read(1)
				b.read(5) // initial display delay and operating point count
				b.read(12)
				level = b.read(5)
				if level > 7 {
					tier = b.read(1)
				}
			}
			if b.bad || level > 23 {
				return info, errInvalid
			}
			t := "M"
			if tier != 0 {
				t = "H"
			}
			info.Codec = fmt.Sprintf("av01.0.%02d%s.08", level, t)
			sequence = true
		case 3, 6: // frame header or frame OBU
			if len(payload) == 0 {
				return info, errInvalid
			}
			if sequence && (reduced || payload[0]&0xf0 == 0x10) {
				key = true
			}
			if kind == 6 {
				picture = true
			}
		case 4: // tile group
			if len(payload) == 0 {
				return info, errInvalid
			}
			picture = true
		}
	}
	info.Recovery = sequence && key && picture
	return info, nil
}

type bits struct {
	data []byte
	pos  int
	bad  bool
}

func (b *bits) read(n int) uint32 {
	if b.bad || n < 0 || n > 32 || b.pos+n > len(b.data)*8 {
		b.bad = true
		return 0
	}
	var v uint32
	for i := 0; i < n; i++ {
		v = v<<1 | uint32((b.data[b.pos/8]>>uint(7-b.pos%8))&1)
		b.pos++
	}
	return v
}
