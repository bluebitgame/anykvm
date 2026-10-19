package capture

import "anykvm/internal/videoenc"

func (d *ffmpegDevice) SupportsGameStreamAV1() bool { return videoenc.SupportsAV1() }
func (s *smartDevice) SupportsGameStreamAV1() bool  { return videoenc.SupportsAV1() }
