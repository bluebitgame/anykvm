package scrcpy

// Minimal H.264 SPS parser: extracts the coded picture dimensions from a codec
// config packet (Annex-B SPS/PPS). Used to track the video size across device
// rotations, when the scrcpy server restarts its encoder and emits a new SPS
// without re-sending codec metadata.

// parseSPSSize scans an Annex-B buffer for an SPS NAL (type 7) and returns the
// decoded picture width/height.
func parseSPSSize(annexb []byte) (w, h int, ok bool) {
	for i := 0; i+3 < len(annexb); {
		j := findStartCode(annexb, i)
		if j < 0 {
			return 0, 0, false
		}
		start := j + 3
		if start >= len(annexb) {
			return 0, 0, false
		}
		end := findStartCode(annexb, start)
		if end < 0 {
			end = len(annexb)
		} else if end > start && annexb[end-1] == 0 {
			end-- // 4-byte start code: trailing zero belongs to the next NAL
		}
		nal := annexb[start:end]
		if len(nal) > 1 && nal[0]&0x1F == 7 {
			return decodeSPS(unescapeRBSP(nal[1:]))
		}
		i = j + 3
	}
	return 0, 0, false
}

// findStartCode returns the index of the next 0x000001 start code at or after
// from, or -1.
func findStartCode(b []byte, from int) int {
	for i := from; i+2 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			return i
		}
	}
	return -1
}

// unescapeRBSP removes H.264 emulation prevention bytes (00 00 03 -> 00 00).
func unescapeRBSP(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		if i >= 2 && data[i] == 3 && data[i-1] == 0 && data[i-2] == 0 {
			continue
		}
		out = append(out, data[i])
	}
	return out
}

// decodeSPS parses a seq_parameter_set RBSP (H.264 spec 7.3.2.1.1) far enough
// to compute the picture dimensions.
func decodeSPS(rbsp []byte) (w, h int, ok bool) {
	r := &bitReader{data: rbsp}

	profileIDC := r.bits(8)
	r.bits(8) // constraint flags + reserved
	r.bits(8) // level_idc
	r.ue()    // seq_parameter_set_id

	chromaFormatIDC := uint32(1) // default 4:2:0
	switch profileIDC {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chromaFormatIDC = r.ue()
		if chromaFormatIDC == 3 {
			r.bit() // separate_colour_plane_flag
		}
		r.ue()            // bit_depth_luma_minus8
		r.ue()            // bit_depth_chroma_minus8
		r.bit()           // qpprime_y_zero_transform_bypass_flag
		if r.bit() == 1 { // seq_scaling_matrix_present_flag
			n := 8
			if chromaFormatIDC == 3 {
				n = 12
			}
			for i := 0; i < n; i++ {
				if r.bit() == 1 { // seq_scaling_list_present_flag[i]
					size := 16
					if i >= 6 {
						size = 64
					}
					skipScalingList(r, size)
				}
			}
		}
	}

	r.ue() // log2_max_frame_num_minus4
	picOrderCntType := r.ue()
	if picOrderCntType == 0 {
		r.ue() // log2_max_pic_order_cnt_lsb_minus4
	} else if picOrderCntType == 1 {
		r.bit()     // delta_pic_order_always_zero_flag
		r.se()      // offset_for_non_ref_pic
		r.se()      // offset_for_top_to_bottom_field
		n := r.ue() // num_ref_frames_in_pic_order_cnt_cycle
		for i := uint32(0); i < n && !r.err; i++ {
			r.se()
		}
	}
	r.ue()  // max_num_ref_frames
	r.bit() // gaps_in_frame_num_value_allowed_flag

	picWidthInMbs := r.ue() + 1
	picHeightInMapUnits := r.ue() + 1
	frameMbsOnly := r.bit()
	if frameMbsOnly == 0 {
		r.bit() // mb_adaptive_frame_field_flag
	}
	r.bit() // direct_8x8_inference_flag

	var cropL, cropR, cropT, cropB uint32
	if r.bit() == 1 { // frame_cropping_flag
		cropL = r.ue()
		cropR = r.ue()
		cropT = r.ue()
		cropB = r.ue()
	}
	if r.err {
		return 0, 0, false
	}

	// Crop units depend on the chroma format (4:2:0 -> 2x2, 4:2:2 -> 2x1,
	// 4:4:4 / monochrome -> 1x1).
	cropX, cropY := uint32(1), uint32(1)
	switch chromaFormatIDC {
	case 1:
		cropX, cropY = 2, 2
	case 2:
		cropX, cropY = 2, 1
	}
	frameHeightMul := uint32(2 - frameMbsOnly)
	cropY *= frameHeightMul

	width := int(picWidthInMbs*16) - int((cropL+cropR)*cropX)
	height := int(frameHeightMul*picHeightInMapUnits*16) - int((cropT+cropB)*cropY)
	if width <= 0 || height <= 0 {
		return 0, 0, false
	}
	return width, height, true
}

// skipScalingList consumes a scaling_list() structure (H.264 spec 7.3.2.1.1.1).
func skipScalingList(r *bitReader, size int) {
	lastScale, nextScale := int32(8), int32(8)
	for i := 0; i < size && !r.err; i++ {
		if nextScale != 0 {
			delta := r.se()
			nextScale = (lastScale + delta + 256) % 256
		}
		if nextScale != 0 {
			lastScale = nextScale
		}
	}
}

// bitReader reads a big-endian bitstream with Exp-Golomb support. Reads past
// the end set err and return 0.
type bitReader struct {
	data []byte
	pos  int // bit position
	err  bool
}

func (r *bitReader) bit() uint32 {
	if r.pos >= len(r.data)*8 {
		r.err = true
		return 0
	}
	b := (r.data[r.pos/8] >> (7 - uint(r.pos%8))) & 1
	r.pos++
	return uint32(b)
}

func (r *bitReader) bits(n int) uint32 {
	var v uint32
	for i := 0; i < n; i++ {
		v = v<<1 | r.bit()
	}
	return v
}

// ue reads an unsigned Exp-Golomb code.
func (r *bitReader) ue() uint32 {
	zeros := 0
	for r.bit() == 0 && !r.err {
		zeros++
		if zeros > 31 {
			r.err = true
			return 0
		}
	}
	if r.err {
		return 0
	}
	return (1<<uint(zeros) - 1) + r.bits(zeros)
}

// se reads a signed Exp-Golomb code.
func (r *bitReader) se() int32 {
	v := r.ue()
	if v%2 == 1 {
		return int32(v/2 + 1)
	}
	return -int32(v / 2)
}
