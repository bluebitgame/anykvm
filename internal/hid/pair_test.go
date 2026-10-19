package hid

import "testing"

func TestSerialStale(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{fmtError("write /dev/cu.usbmodem14101: device not configured"), true},
		{fmtError("input/output error"), false},
		{fmtError("bad file descriptor"), true},
	}
	for _, tc := range cases {
		if got := SerialStale(tc.err); got != tc.want {
			t.Errorf("SerialStale(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

type fmtError string

func (e fmtError) Error() string { return string(e) }
