package rdp

import "testing"

func TestClientOSName(t *testing.T) {
	for _, tt := range []struct {
		major, minor uint16
		want         string
	}{
		{0, 0, "Unspecified"},
		{1, 0, "Windows"},
		{1, 1, "Windows 3.1x"},
		{1, 2, "Windows 95"},
		{1, 3, "Windows NT"},
		{1, 9, "Windows RT"},
		{1, 0xffff, "Windows"},
		{2, 4, "OS/2"},
		{3, 6, "Macintosh"},
		{4, 7, "UNIX"},
		{5, 0, "iOS"},
		{6, 0, "macOS"},
		{7, 0, "Android"},
		{8, 0, "Chrome OS"},
		{0xffff, 42, "Unknown (65535/42)"},
	} {
		if got := clientOSName(tt.major, tt.minor); got != tt.want {
			t.Errorf("clientOSName(%d, %d) = %q, want %q", tt.major, tt.minor, got, tt.want)
		}
	}
}
