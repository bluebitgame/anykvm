package capture

// preferredCaptureFormat changes only an unspecified format at a fixed size.
// Never switch the user's explicitly chosen subtype or resolution.
func preferredCaptureFormat(modes []VideoMode, w, h int, fps float64, codec string, decode bool) string {
	if w <= 0 || h <= 0 || decode {
		return ""
	}
	wanted := "nv12"
	if codec == "" || codec == "mjpeg" {
		wanted = "mjpeg"
	} else if codec != "h264" && codec != "hevc" && codec != "av1" {
		return ""
	}
	for _, m := range modes {
		if m.Width == w && m.Height == h && m.PixelFormat == wanted && (fps <= 0 || (fps >= m.MinFPS-0.001 && fps <= m.MaxFPS+0.001)) {
			return wanted
		}
	}
	return ""
}
