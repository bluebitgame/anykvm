package videodec

import (
	"fmt"
	"io"
)

// Bounded SPS reader shared in behavior with the CoreDevice video path.
type bitReader struct {
	data []byte
	bit  int
	err  error
}

func (r *bitReader) read(n int) uint64 {
	if n < 0 || n > 64 || r.bit+n > len(r.data)*8 {
		r.err = io.ErrUnexpectedEOF
		return 0
	}
	var v uint64
	for i := 0; i < n; i++ {
		v = v<<1 | uint64(r.data[r.bit/8]>>uint(7-r.bit%8)&1)
		r.bit++
	}
	return v
}
func (r *bitReader) ue() uint64 {
	n := 0
	for r.err == nil && r.read(1) == 0 {
		n++
		if n > 31 {
			r.err = fmt.Errorf("video: invalid SPS Exp-Golomb value")
			return 0
		}
	}
	return (1 << uint(n)) - 1 + r.read(n)
}

type H264Info struct{ Width, Height, Profile, Level int }

func h264Signed(r *bitReader) int64 {
	v := r.ue()
	if v&1 != 0 {
		return int64(v/2 + 1)
	}
	return -int64(v / 2)
}

// Read enough SPS syntax to get the visible dimensions; honor crop units,
// interlaced frames and High-profile scaling lists, with bounded network reads.
func ParseSPS(nal []byte) (H264Info, error) {
	var info H264Info
	if len(nal) < 4 || nal[0]&128 != 0 || nal[0]&31 != 7 {
		return info, fmt.Errorf("video: missing H.264 SPS")
	}
	rbsp := make([]byte, 0, len(nal))
	zeros := 0
	for _, b := range nal[1:] {
		if zeros == 2 && b == 3 {
			zeros = 0
			continue
		}
		rbsp = append(rbsp, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	r := bitReader{data: rbsp}
	info.Profile = int(r.read(8))
	r.read(8)
	info.Level = int(r.read(8))
	if r.ue() > 31 {
		return info, fmt.Errorf("video: invalid H.264 SPS ID")
	}
	chroma, separate := uint64(1), false
	switch info.Profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chroma = r.ue()
		if chroma > 3 {
			return info, fmt.Errorf("video: invalid H.264 chroma format")
		}
		if chroma == 3 {
			separate = r.read(1) != 0
		}
		if r.ue() > 6 || r.ue() > 6 {
			return info, fmt.Errorf("video: invalid H.264 bit depth")
		}
		r.read(1)
		if r.read(1) != 0 {
			count := 8
			if chroma == 3 {
				count = 12
			}
			for i := 0; i < count; i++ {
				if r.read(1) != 0 {
					size := 16
					if i >= 6 {
						size = 64
					}
					last, next := int64(8), int64(8)
					for j := 0; j < size && r.err == nil; j++ {
						if next != 0 {
							next = (last + h264Signed(&r)) % 256
							if next < 0 {
								next += 256
							}
						}
						if next != 0 {
							last = next
						}
					}
				}
			}
		}
	case 66, 77, 88:
	default:
		return info, fmt.Errorf("video: unsupported H.264 SPS profile %d", info.Profile)
	}
	if r.ue() > 12 {
		return info, fmt.Errorf("video: invalid H.264 frame number size")
	}
	switch r.ue() {
	case 0:
		if r.ue() > 12 {
			return info, fmt.Errorf("video: invalid H.264 POC size")
		}
	case 1:
		r.read(1)
		h264Signed(&r)
		h264Signed(&r)
		count := r.ue()
		if count > 255 {
			return info, fmt.Errorf("video: invalid H.264 POC cycle")
		}
		for i := uint64(0); i < count && r.err == nil; i++ {
			h264Signed(&r)
		}
	case 2:
	default:
		return info, fmt.Errorf("video: invalid H.264 POC type")
	}
	r.ue()
	r.read(1)
	mbW, mbH := r.ue()+1, r.ue()+1
	if mbW > 512 || mbH > 512 {
		return info, fmt.Errorf("video: H.264 dimensions exceed limit")
	}
	frameOnly := r.read(1)
	if frameOnly == 0 {
		r.read(1)
	}
	r.read(1)
	var l, rr, t, b uint64
	if r.read(1) != 0 {
		l = r.ue()
		rr = r.ue()
		t = r.ue()
		b = r.ue()
	}
	if r.err != nil {
		return info, r.err
	}
	x, y := uint64(1), uint64(1)
	if !separate {
		switch chroma {
		case 1:
			x, y = 2, 2
		case 2:
			x = 2
		}
	}
	width, height := mbW*16, (2-frameOnly)*mbH*16
	cropW, cropH := (l+rr)*x, (t+b)*y*(2-frameOnly)
	if cropW >= width || cropH >= height || height > 8192 {
		return info, fmt.Errorf("video: invalid H.264 crop/dimensions")
	}
	info.Width, info.Height = int(width-cropW), int(height-cropH)
	return info, nil
}
