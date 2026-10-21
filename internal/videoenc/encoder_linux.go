//go:build linux && cgo

package videoenc

import (
	"anykvm/internal/avmedia"
	"fmt"
	"log"
)

func newPlatformEncoder(o Options) (Encoder, error) {
	if o.Codec != "h264" && o.Codec != "hevc" && o.Codec != "av1" {
		return nil, ErrUnavailable
	}
	// Native-only builds use Vulkan Video directly, without library GPU backends.
	if !avmedia.Enabled {
		e, vulkanErr := openLinuxVulkan(o)
		if vulkanErr == nil {
			return e, nil
		}
		log.Printf("encoder Vulkan Video: %v", vulkanErr)
		if o.Codec == "h264" {
			if e, err := openRaspberryPi(o); err == nil {
				return e, nil
			}
		}
		return nil, fmt.Errorf("%w: Linux native %s: %v", ErrUnavailable, o.Codec, vulkanErr)
	}

	nvidia, devices := linuxH264Hardware("/sys", "/dev/dri")
	return selectLinuxEncoder(o, nvidia, devices, openRaspberryPi, openAV)
}
