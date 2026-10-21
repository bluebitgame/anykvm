//go:build !darwin || !cgo

package capture

import "anykvm/internal/videoenc"

func (s *smartDevice) SupportsGameStreamHEVC() bool { return videoenc.SupportsHEVC() }
