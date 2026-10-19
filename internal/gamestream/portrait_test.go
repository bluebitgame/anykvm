package gamestream

import "testing"

func TestPortraitStreamDimensions(t *testing.T) {
	for _, tc := range []struct {
		w, h  int
		valid bool
	}{
		{1920, 1080, true}, {3840, 2160, true}, {2160, 3840, true},
		{1328, 2896, true}, {2896, 1328, true}, {3840, 2162, true},
		{2162, 3840, true}, {3842, 2160, true}, {2160, 3842, true},
		{7680, 4320, true}, {4320, 7680, true}, {32, 2896, true},
		{1329, 2896, false}, {1328, 2897, false}, {0, 2896, false}, {-2, 2896, false},
	} {
		err := validateOptions(streamOptions{Width: tc.w, Height: tc.h, FPS: 30, Bitrate: 5000, PacketSize: 1024, MinFEC: 2, AudioDuration: 5})
		if (err == nil) != tc.valid {
			t.Fatalf("%dx%d: %v", tc.w, tc.h, err)
		}
	}
}
