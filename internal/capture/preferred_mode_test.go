package capture

import "testing"

func TestCaptureFormatPreferenceKeepsSizeAndRate(t *testing.T) {
	modes := []VideoMode{{Width: 1920, Height: 1080, PixelFormat: "nv12", MinFPS: 30, MaxFPS: 60}, {Width: 1920, Height: 1080, PixelFormat: "mjpeg", MinFPS: 30, MaxFPS: 60}}
	for _, tc := range []struct {
		codec  string
		w, h   int
		fps    float64
		decode bool
		want   string
	}{
		{"h264", 1920, 1080, 60, false, "nv12"}, {"mjpeg", 1920, 1080, 60, false, "mjpeg"}, {"h264", 1920, 1080, 120, false, ""},
		{"h264", 2560, 1440, 30, false, ""}, {"rawvideo", 1920, 1080, 30, false, ""}, {"mjpeg", 1920, 1080, 30, true, ""}, {"h264", 0, 0, 30, false, ""},
	} {
		if got := preferredCaptureFormat(modes, tc.w, tc.h, tc.fps, tc.codec, tc.decode); got != tc.want {
			t.Fatalf("%+v: got %q", tc, got)
		}
	}
}
