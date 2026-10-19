package scrcpy

import "testing"

// bitWriter builds an RBSP bitstream for test SPS construction.
type bitWriter struct {
	data []byte
	nbit uint
}

func (w *bitWriter) bit(b uint32) {
	if w.nbit%8 == 0 {
		w.data = append(w.data, 0)
	}
	if b != 0 {
		w.data[len(w.data)-1] |= 1 << (7 - w.nbit%8)
	}
	w.nbit++
}

func (w *bitWriter) bits(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		w.bit((v >> uint(i)) & 1)
	}
}

// ue writes an unsigned Exp-Golomb code.
func (w *bitWriter) ue(v uint32) {
	v++
	n := 0
	for tmp := v; tmp > 1; tmp >>= 1 {
		n++
	}
	for i := 0; i < n; i++ {
		w.bit(0)
	}
	w.bits(v, n+1)
}

// se writes a signed Exp-Golomb code.
func (w *bitWriter) se(v int32) {
	if v > 0 {
		w.ue(uint32(2*v - 1))
	} else {
		w.ue(uint32(-2 * v))
	}
}

// buildSPS encodes a seq_parameter_set RBSP for the given coded size.
// mbW/mbH are macroblock counts; crop* are cropping offsets in crop units.
func buildSPS(profile uint32, mbW, mbH, cropL, cropR, cropT, cropB uint32) []byte {
	w := &bitWriter{}
	w.bits(profile, 8) // profile_idc
	w.bits(0, 8)       // constraint flags + reserved
	w.bits(31, 8)      // level_idc
	w.ue(0)            // seq_parameter_set_id
	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		w.ue(1)  // chroma_format_idc = 4:2:0
		w.ue(0)  // bit_depth_luma_minus8
		w.ue(0)  // bit_depth_chroma_minus8
		w.bit(0) // qpprime_y_zero_transform_bypass_flag
		w.bit(0) // seq_scaling_matrix_present_flag
	}
	w.ue(4)  // log2_max_frame_num_minus4
	w.ue(0)  // pic_order_cnt_type
	w.ue(4)  // log2_max_pic_order_cnt_lsb_minus4
	w.ue(1)  // max_num_ref_frames
	w.bit(0) // gaps_in_frame_num_value_allowed_flag
	w.ue(mbW - 1)
	w.ue(mbH - 1)
	w.bit(1) // frame_mbs_only_flag
	w.bit(1) // direct_8x8_inference_flag
	crop := cropL|cropR|cropT|cropB != 0
	if crop {
		w.bit(1)
		w.ue(cropL)
		w.ue(cropR)
		w.ue(cropT)
		w.ue(cropB)
	} else {
		w.bit(0)
	}
	w.bit(0) // vui_parameters_present_flag
	w.bit(1) // rbsp_stop_one_bit
	return w.data
}

// escapeRBSP inserts emulation prevention bytes (00 00 -> 00 00 03).
func escapeRBSP(rbsp []byte) []byte {
	out := make([]byte, 0, len(rbsp))
	zeros := 0
	for _, b := range rbsp {
		if zeros >= 2 && b <= 3 {
			out = append(out, 3)
			zeros = 0
		}
		out = append(out, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// annexbSPS wraps an SPS RBSP into an Annex-B config packet (with a PPS after
// it, as the scrcpy server sends).
func annexbSPS(rbsp []byte) []byte {
	buf := []byte{0, 0, 0, 1, 0x67}
	buf = append(buf, escapeRBSP(rbsp)...)
	buf = append(buf, 0, 0, 0, 1, 0x68, 0xCE, 0x3C, 0x80) // dummy PPS
	return buf
}

func TestParseSPSSize(t *testing.T) {
	cases := []struct {
		name    string
		profile uint32
		mbW     uint32
		mbH     uint32
		crop    [4]uint32 // L, R, T, B in crop units (4:2:0 -> 2px per unit)
		wantW   int
		wantH   int
	}{
		{"1920x1080 baseline", 66, 120, 68, [4]uint32{0, 0, 0, 4}, 1920, 1080},
		{"1920x1080 high", 100, 120, 68, [4]uint32{0, 0, 0, 4}, 1920, 1080},
		{"1080x2340 high (phone portrait)", 100, 68, 147, [4]uint32{0, 4, 0, 6}, 1080, 2340},
		{"1280x720 no crop", 66, 80, 45, [4]uint32{}, 1280, 720},
		{"1354x642 odd crop", 100, 85, 41, [4]uint32{0, 3, 0, 7}, 1354, 642},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pkt := annexbSPS(buildSPS(tc.profile, tc.mbW, tc.mbH, tc.crop[0], tc.crop[1], tc.crop[2], tc.crop[3]))
			w, h, ok := parseSPSSize(pkt)
			if !ok {
				t.Fatalf("parseSPSSize failed")
			}
			if w != tc.wantW || h != tc.wantH {
				t.Fatalf("got %dx%d, want %dx%d", w, h, tc.wantW, tc.wantH)
			}
		})
	}
}

func TestParseSPSSizeRejectsGarbage(t *testing.T) {
	if _, _, ok := parseSPSSize([]byte{0, 0, 0, 1, 0x68, 0xCE, 0x3C, 0x80}); ok {
		t.Fatalf("expected failure on PPS-only packet")
	}
	if _, _, ok := parseSPSSize([]byte{1, 2, 3, 4}); ok {
		t.Fatalf("expected failure on garbage")
	}
}

func TestIsAndroidDevice(t *testing.T) {
	for id, want := range map[string]bool{
		"adb":          true,
		"adb:emu-5554": true,
		"android":      false,
		"screen":       false,
		"0":            false,
		"/dev/video0":  false,
	} {
		if got := IsAndroidDevice(id); got != want {
			t.Errorf("IsAndroidDevice(%q) = %v, want %v", id, got, want)
		}
	}
	if got := DeviceSerial("adb:abc123"); got != "abc123" {
		t.Errorf("DeviceSerial = %q, want abc123", got)
	}
	if got := DeviceSerial("adb"); got != "" {
		t.Errorf("DeviceSerial = %q, want empty", got)
	}
}
