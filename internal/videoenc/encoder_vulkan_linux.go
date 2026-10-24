//go:build linux && cgo

package videoenc

import (
	"anykvm/internal/dmabuf"
	"anykvm/internal/gpuconv"
	"anykvm/internal/logging"
	"errors"
	"fmt"
)

type vulkanEncoder struct {
	encoder *gpuconv.VideoEncoder
	options Options
}

func openLinuxVulkan(o Options) (Encoder, error) {
	if o.Codec != "h264" && o.Codec != "hevc" && o.Codec != "av1" {
		return nil, fmt.Errorf("unsupported native Vulkan Video codec %s", o.Codec)
	}
	bitrate := o.Bitrate
	if bitrate == 0 {
		bitrate = 12000
	}
	e, err := gpuconv.NewVideoEncoderCodec(o.DRMDeviceMajor, o.DRMDeviceMinor, o.Width, o.Height, o.Rate.Num, o.Rate.Den, bitrate, o.Codec)
	if err != nil {
		return nil, err
	}
	return &vulkanEncoder{encoder: e, options: o}, nil
}
func (e *vulkanEncoder) Encode(pixels []byte, force bool) ([]byte, error) {
	if len(pixels) != e.options.Width*e.options.Height*3/2 {
		return nil, fmt.Errorf("invalid Vulkan NV12 frame size")
	}
	return e.encoder.EncodeNV12(pixels, force)
}
func (e *vulkanEncoder) EncodeDMABUF(f dmabuf.Frame, force bool) ([]byte, bool, error) {
	if e.options.DRMDeviceMajor != 0 && (f.DeviceMajor != e.options.DRMDeviceMajor || f.DeviceMinor != e.options.DRMDeviceMinor) {
		return nil, true, fmt.Errorf("Vulkan capture and encoder DRM devices differ")
	}
	data, err := e.encoder.EncodeSource(f, force)
	if errors.Is(err, gpuconv.ErrUnavailable) {
		logging.Infof("videoenc: NV12 DMA-BUF import unavailable: %v", err)
		return nil, false, ErrUnavailable
	}
	return data, true, err
}
func (e *vulkanEncoder) EncodeLast(force bool) ([]byte, error) { return e.encoder.EncodeLast(force) }
func (e *vulkanEncoder) Close()                                { e.encoder.Close() }
func (e *vulkanEncoder) Name() string                          { return e.options.Codec + "_vulkan_native" }
