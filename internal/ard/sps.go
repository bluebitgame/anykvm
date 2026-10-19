package ard

import (
	"fmt"
	"io"
)

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
			r.err = fmt.Errorf("ard: invalid SPS Exp-Golomb value")
			return 0
		}
	}
	return (1 << uint(n)) - 1 + r.read(n)
}

type hevcInfo struct {
	Profile, Level, Chroma, DepthLuma, DepthChroma int
	Width, Height                                  int
}

func parseSPS(nal []byte) (hevcInfo, error) {
	var info hevcInfo
	if len(nal) < 3 || nal[0]>>1&63 != 33 {
		return info, fmt.Errorf("ard: missing HEVC SPS")
	}
	rbsp := make([]byte, 0, len(nal))
	zeros := 0
	for _, v := range nal[2:] {
		if zeros == 2 && v == 3 {
			zeros = 0
			continue
		}
		rbsp = append(rbsp, v)
		if v == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	r := bitReader{data: rbsp}
	r.read(4)
	layers := int(r.read(3))
	r.read(1)
	r.read(3)
	info.Profile = int(r.read(5))
	r.read(32)
	r.read(48)
	info.Level = int(r.read(8))
	var profile, level [7]bool
	for i := 0; i < layers; i++ {
		profile[i] = r.read(1) != 0
		level[i] = r.read(1) != 0
	}
	if layers > 0 {
		r.read((8 - layers) * 2)
	}
	for i := 0; i < layers; i++ {
		if profile[i] {
			r.read(64)
			r.read(24)
		}
		if level[i] {
			r.read(8)
		}
	}
	r.ue()
	info.Chroma = int(r.ue())
	if info.Chroma > 3 {
		return info, fmt.Errorf("ard: invalid HEVC chroma format")
	}
	separate := false
	if info.Chroma == 3 {
		separate = r.read(1) != 0
	}
	w, h := int(r.ue()), int(r.ue())
	if r.read(1) != 0 {
		l, right, top, bottom := int(r.ue()), int(r.ue()), int(r.ue()), int(r.ue())
		sx, sy := 1, 1
		if !separate && (info.Chroma == 1 || info.Chroma == 2) {
			sx = 2
		}
		if !separate && info.Chroma == 1 {
			sy = 2
		}
		w -= (l + right) * sx
		h -= (top + bottom) * sy
	}
	info.Width, info.Height = w, h
	info.DepthLuma = 8 + int(r.ue())
	info.DepthChroma = 8 + int(r.ue())
	if r.err != nil {
		return info, r.err
	}
	if !validSize(w, h) || info.DepthLuma > 16 || info.DepthChroma > 16 {
		return info, fmt.Errorf("ard: invalid HEVC SPS dimensions/depth")
	}
	return info, nil
}
