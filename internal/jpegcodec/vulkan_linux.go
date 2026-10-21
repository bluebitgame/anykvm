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

//go:embed shaders/jpeg420_scanSegmentGroups.spv
var jpegScanGroupsSPIRV []byte

//go:embed shaders/jpeg420_addSegmentOffsets.spv
var jpegAddOffsetsSPIRV []byte

//go:embed shaders/jpeg420_packJPEG.spv
var jpegPackSPIRV []byte

//go:embed shaders/jpeg420_compareTiles.spv
var jpegCompareSPIRV []byte

//go:embed shaders/jpeg420_prepareWork.spv
var jpegWorkSPIRV []byte

//go:embed shaders/jpeg420_selectEntropy.spv
var jpegSelectEntropySPIRV []byte

//go:embed shaders/jpeg420_prepareEntropy.spv
var jpegPrepareEntropySPIRV []byte

// VulkanEncoder shares standalone JPEG and changed-MCU compute modes with D3D11.
// The capture owner serializes EncodeSource and Close.
type VulkanEncoder struct {
	encoder    *gpuconv.JPEGEncoder
	parameters jpegFrameParameters
	regions    *RestartJPEG
}

func NewVulkanEncoder(major, minor uint32) (*VulkanEncoder, error) {
	encoder, err := gpuconv.NewJPEGEncoder(major, minor, [10][]byte{
		jpegSPIRV, jpegEntropySPIRV, jpegIndexSPIRV, jpegPackSPIRV, jpegCompareSPIRV, jpegWorkSPIRV, jpegScanGroupsSPIRV, jpegAddOffsetsSPIRV, jpegSelectEntropySPIRV, jpegPrepareEntropySPIRV,
	})
	if err != nil {
		return nil, err
	}
	return &VulkanEncoder{encoder: encoder}, nil
}

func (e *VulkanEncoder) EncodeSource(source dmabuf.Frame, width, height, quality int) ([]byte, error) {
	if e == nil || e.encoder == nil {
		return nil, fmt.Errorf("Vulkan JPEG encoder closed")
	}
	if err := e.parameters.prepare(width, height, quality); err != nil {
		return nil, err
	}
	e.regions = nil
	return e.encoder.EncodeSource(source, width, height, &e.parameters.quantization, &jpegPackedEntropyCodes, e.parameters.header)
}

// EncodeRegions reads only changed compressed MCU records, retaining an immutable
// CPU segment snapshot for consumers that can skip intermediate captures.
func (e *VulkanEncoder) EncodeRegions(source dmabuf.Frame, width, height, quality int) (*RestartJPEG, error) {
	if e == nil || e.encoder == nil {
		return nil, fmt.Errorf("Vulkan JPEG encoder closed")
	}
	if err := e.parameters.prepare(width, height, quality); err != nil {
		return nil, err
	}
	packet, err := e.encoder.EncodeRegions(source, width, height, &e.parameters.quantization, &jpegPackedEntropyCodes, e.parameters.header)
	if err != nil {
		return nil, err
	}
	next, err := updateGPURegions(e.regions, e.parameters.header, packet, width, height)
	if err == nil {
		e.regions = next
	}
	return next, err
}

func (e *VulkanEncoder) Close() {
	if e != nil && e.encoder != nil {
		e.encoder.Close()
		e.encoder = nil
		e.regions = nil
	}
}

// DRMDevice returns the Vulkan renderer's DRM device, not the KMS source.
func (e *VulkanEncoder) DRMDevice() (uint32, uint32) {
	if e == nil || e.encoder == nil {
		return 0, 0
	}
	return e.encoder.DRMDevice()
}
