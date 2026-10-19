package scrcpy

// parseHEVCSPSSize tracks rotations on servers predating session metadata.
func parseHEVCSPSSize(annexb []byte) (int, int, bool) {
	for offset := 0; ; {
		start := findStartCode(annexb, offset)
		if start < 0 {
			return 0, 0, false
		}
		start += 3
		end := findStartCode(annexb, start)
		if end < 0 {
			end = len(annexb)
		}
		if end > start+2 && (annexb[start]>>1)&63 == 33 {
			return decodeHEVCSPS(unescapeRBSP(annexb[start+2 : end]))
		}
		offset = end
	}
}

func decodeHEVCSPS(rbsp []byte) (int, int, bool) {
	r := &bitReader{data: rbsp}
	r.bits(4) // sps_video_parameter_set_id
	layers := int(r.bits(3))
	r.bit() // sps_temporal_id_nesting_flag
	if layers > 6 {
		return 0, 0, false
	}
	r.bits(32) // general profile (8), compatibility flags (32), constraints (48), level (8)
	r.bits(32)
	r.bits(32)
	var profiles, levels [7]uint32
	for i := 0; i < layers; i++ {
		profiles[i], levels[i] = r.bit(), r.bit()
	}
	if layers > 0 {
		r.bits((8 - layers) * 2)
	}
	for i := 0; i < layers; i++ {
		if profiles[i] != 0 {
			r.bits(32)
			r.bits(32)
			r.bits(24)
		}
		if levels[i] != 0 {
			r.bits(8)
		}
	}
	r.ue() // sps_seq_parameter_set_id
	chroma := r.ue()
	if chroma > 3 {
		return 0, 0, false
	}
	separate := chroma == 3 && r.bit() != 0
	w, h := int64(r.ue()), int64(r.ue())
	if r.bit() != 0 {
		left, right, top, bottom := int64(r.ue()), int64(r.ue()), int64(r.ue()), int64(r.ue())
		sx, sy := int64(1), int64(1)
		if !separate && (chroma == 1 || chroma == 2) {
			sx = 2
		}
		if !separate && chroma == 1 {
			sy = 2
		}
		w -= (left + right) * sx
		h -= (top + bottom) * sy
	}
	if r.err || w <= 0 || h <= 0 || w > 65535 || h > 65535 {
		return 0, 0, false
	}
	return int(w), int(h), true
}
