//go:build darwin && cgo

package capture

import "testing"

func TestCaptureModeSelection(t *testing.T) {
	modes := []VideoMode{
		{1920, 1080, "yuyv422", 30, 60}, {1920, 1080, "nv12", 30, 60}, {2560, 1440, "nv12", 30, 30},
	}
	for _, tc := range []struct {
		value, format string
		width         int
	}{
		{"1920x1080@60:nv12", "nv12", 1920}, {"1920x1080@30:yuyv422", "yuyv422", 1920},
		{"2560x1440:nv12", "nv12", 2560},
	} {
		request, _ := ParseMode(tc.value)
		got, err := chooseVideoMode(modes, request)
		if err != nil || got.PixelFormat != tc.format || got.Width != tc.width {
			t.Fatalf("%s: %+v %v", tc.value, got, err)
		}
	}
	for _, value := range []string{"1920x1080@120:nv12", "1920x1080@15:nv12", "1280x720@60:nv12", "2560x1440@60:nv12", "1920x1080@60:mjpeg"} {
		request, _ := ParseMode(value)
		if _, err := chooseVideoMode(modes, request); err == nil {
			t.Errorf("unsupported mode %q accepted", value)
		}
	}
	if nativeFormatCode("nv12") != 0x34323076 || nativeFormatName(0x79757673) != "yuyv422" {
		t.Fatal("incorrect native FourCC mapping")
	}
	unknown := uint32(0x76323130)
	if nativeFormatCode(nativeFormatName(unknown)) != unknown {
		t.Fatal("unknown FourCC lost in roundtrip")
	}
}
