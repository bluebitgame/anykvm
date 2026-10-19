//go:build linux && cgo

package jpegcodec

import (
	"anykvm/internal/dmabuf"
	"anykvm/internal/gpuconv"
	_ "embed"
	"fmt"
)

//go:embed shaders/jpeg420.spv
var jpegSPIRV []byte

//go:embed shaders/jpeg420_encodeMCUs.spv
var jpegEntropySPIRV []byte

//go:embed shaders/jpeg420_indexSegments.spv
var jpegIndexSPIRV []byte

//go:embed shaders/jpeg420_packJPEG.spv
var jpegPackSPIRV []byte

// VulkanEncoder uses the same full JPEG compute pipeline as D3D11.
// The capture owner serializes EncodeSource and Close.
type VulkanEncoder struct{ encoder *gpuconv.JPEGEncoder }

func NewVulkanEncoder(major, minor uint32) (*VulkanEncoder, error) {
	encoder, err := gpuconv.NewJPEGEncoder(major, minor, [4][]byte{jpegSPIRV, jpegEntropySPIRV, jpegIndexSPIRV, jpegPackSPIRV})
	if err != nil {
		return nil, err
	}
	return &VulkanEncoder{encoder: encoder}, nil
}

func (e *VulkanEncoder) EncodeSource(source dmabuf.Frame, width, height, quality int) ([]byte, error) {
	if e == nil || e.encoder == nil {
		return nil, fmt.Errorf("Vulkan JPEG encoder closed")
	}
	quantization, huffman, header, err := gpuJPEGFrame(width, height, quality)
	if err != nil {
		return nil, err
	}
	return e.encoder.EncodeSource(source, width, height, quantization, huffman, header)
}

func (e *VulkanEncoder) Close() {
	if e != nil && e.encoder != nil {
		e.encoder.Close()
		e.encoder = nil
	}
}
