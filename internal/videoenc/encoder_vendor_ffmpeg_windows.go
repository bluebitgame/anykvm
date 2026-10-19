//go:build windows && cgo && ffmpeg

package videoenc

// Tagged builds share libavcodec's vendor backends and exclude the native SDK bridges.
func openWindowsVendor(o Options, name string) (Encoder, error) {
	// This bridge uploads CPU pixels and cannot bind a desktop texture by LUID.
	// Fail explicitly so selection reports the unbound compatibility fallback.
	if o.WindowsAdapter.Set {
		return nil, ErrUnavailable
	}
	if o.Codec == "h264" || o.Codec == "av1" {
		for _, suffix := range []string{"_nvenc", "_amf", "_qsv"} {
			if name == o.Codec+suffix {
				return openAV(o, name, "")
			}
		}
	}
	return nil, ErrUnavailable
}
