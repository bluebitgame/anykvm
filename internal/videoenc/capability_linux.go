//go:build linux && cgo

package videoenc

import (
	"anykvm/internal/avmedia"
	"anykvm/internal/gpuconv"
)

func platformSupportsCodec(codec string) bool {
	if !avmedia.Enabled {
		return gpuconv.SupportsVideoCodec(codec)
	}
	return librarySupportsCodec(codec)
}
