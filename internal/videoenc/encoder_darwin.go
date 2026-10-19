//go:build darwin && cgo

package videoenc

func newPlatformEncoder(o Options) (Encoder, error) {
	if o.Codec == "av1" {
		return nil, ErrUnavailable
	}
	e, err := openNative(o, "videotoolbox", "")
	if err == nil {
		return e, nil
	}
	name := "h264_videotoolbox"
	if o.Codec == "hevc" {
		name = "hevc_videotoolbox"
	}
	if e, liberr := openAV(o, name, ""); liberr == nil {
		return e, nil
	}
	if o.Codec == "h264" {
		return openAV(o, "libx264", "")
	}
	return nil, err
}
