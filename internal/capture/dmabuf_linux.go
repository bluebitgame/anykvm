//go:build linux && cgo

package capture

import (
	"anykvm/internal/gpuconv"
	"anykvm/internal/jpegcodec"
	"sync"
)

var dmaHEVCAvailable = sync.OnceValue(func() bool { return gpuconv.SupportsVideoCodec("hevc") })
var dmaAV1Available = sync.OnceValue(func() bool { return gpuconv.SupportsVideoCodec("av1") })

func supportsDMABUFVideoCodec(codec string) bool {
	switch codec {
	case "hevc":
		return dmaHEVCAvailable()
	case "av1":
		return dmaAV1Available()
	}
	return false
}

func newDMABUFJPEG(major, minor uint32) (dmabufJPEGEncoder, error) {
	encoder, err := jpegcodec.NewVulkanEncoder(major, minor)
	if err != nil {
		return nil, err
	}
	return encoder, nil
}
