package videoenc

import "log"

func selectLinuxEncoder(o Options, nvidia bool, devices []string,
	openNative func(Options) (Encoder, error),
	openLibrary func(Options, string, string) (Encoder, error)) (Encoder, error) {
	if o.Codec != "h264" && o.Codec != "hevc" && o.Codec != "av1" {
		return nil, ErrUnavailable
	}
	if nvidia {
		if e, err := openLibrary(o, o.Codec+"_nvenc", ""); err == nil {
			return e, nil
		} else {
			log.Printf("encoder NVENC: %v", err)
		}
	}
	for _, device := range devices {
		if e, err := openLibrary(o, o.Codec+"_vaapi", device); err == nil {
			return e, nil
		} else {
			log.Printf("encoder VAAPI %s: %v", device, err)
		}
	}
	if o.Codec == "h264" {
		if e, err := openNative(o); err == nil {
			return e, nil
		}
		return openLibrary(o, "libx264", "")
	}
	return nil, ErrUnavailable
}
