package capture

import "testing"

func TestScreenEncoderFrameLimit(t *testing.T) {
	for _, tc := range []struct {
		codec            string
		configured, want int
	}{
		{"h264", 0, 2}, {"h264", 1, 1}, {"h264", 3, 3}, {"h264", 8, 8},
		{"hevc", 0, 4}, {"hevc", 1, 4}, {"hevc", 3, 4}, {"hevc", 8, 8},
	} {
		got, err := screenEncoderFrameLimit(tc.codec, tc.configured)
		if err != nil || got != tc.want {
			t.Fatalf("codec=%s configured=%d: got=%d err=%v want=%d", tc.codec, tc.configured, got, err, tc.want)
		}
	}
	for _, invalid := range []int{-1, 9} {
		if _, err := screenEncoderFrameLimit("h264", invalid); err == nil {
			t.Fatalf("accepted invalid limit %d", invalid)
		}
	}
}
