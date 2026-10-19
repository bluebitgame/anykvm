package capture

import "testing"

func TestCaptureModeParsing(t *testing.T) {
	for _, tc := range []struct {
		value string
		mode  Mode
	}{
		{"", Mode{}},
		{"1920x1080@59.94:guid_123456780001000280001234567890ab", Mode{Width: 1920, Height: 1080, FPS: 59.94, PixelFormat: "guid_123456780001000280001234567890ab"}}, {"1920x1080@60:nv12", Mode{Width: 1920, Height: 1080, FPS: 60, PixelFormat: "nv12"}},
		{"1920x1080:nv12", Mode{Width: 1920, Height: 1080, PixelFormat: "nv12"}},
		{"@59.94", Mode{FPS: 59.94}}, {" AUTO@30 ", Mode{FPS: 30, FollowClient: true}},
		{"1920x1080@30:fourcc_76323130", Mode{Width: 1920, Height: 1080, FPS: 30, PixelFormat: "fourcc_76323130"}},
	} {
		got, err := ParseMode(tc.value)
		if err != nil || got != tc.mode {
			t.Fatalf("ParseMode(%q)=%+v,%v", tc.value, got, err)
		}
	}
	for _, value := range []string{"1920x1080@60:fake", "1920x1080:guid_123", "1920x1080:guid_123456780001000280001234567890azz", "1920x1080@0", "1920x0", "1920x1080@60oops", "auto:nv12", "@1001", "1920x1080@60:nv12:extra", "@", "1920x1080@-1", "9999999999999999999999x1080"} {
		if _, err := ParseMode(value); err == nil {
			t.Errorf("accepted invalid mode %q", value)
		}
	}
}

func TestWindowsInputFormatsParse(t *testing.T) {
	for _, format := range []string{"nv12", "nv12_full", "yuyv422", "uyvy422", "mjpeg", "h264", "bgra", "bgr0", "bgr24", "i420", "iyuv", "yv12", "p010", "p016", "y210", "y216", "v210", "v216"} {
		if _, err := ParseMode("1920x1080@59.94:" + format); err != nil {
			t.Fatal(format, err)
		}
	}
}
