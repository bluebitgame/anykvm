package capture

import "anykvm/internal/videoenc"

func (d *ffmpegDevice) SupportsGameStreamAV1() bool { return videoenc.SupportsAV1() }
func (s *smartDevice) SupportsGameStreamAV1() bool  { return videoenc.SupportsAV1() }

func (d *ffmpegDevice) SupportsGameStreamHEVC() bool { return videoenc.SupportsHEVC() }

func (d *ffmpegDevice) SupportsVideoCodec(codec string) bool {
	switch codec {
	case "hevc":
		return videoenc.SupportsHEVC()
	case "av1":
		return videoenc.SupportsAV1()
	default:
		return true
	}
}
