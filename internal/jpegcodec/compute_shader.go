package jpegcodec

import (
	"anykvm/internal/gpuconv"
	_ "embed"
)

// Shared by GPU backends: D3D11 uses DXBC; Vulkan can compile SPIR-V.
//
//go:embed shaders/jpeg420.hlsl
var jpegComputeSource string

var jpegComputeShader = gpuconv.WithTileCompare(jpegComputeSource, "compareTiles")
