package jpegcodec

import _ "embed"

// Shared by GPU backends: D3D11 compiles DXBC; Vulkan can compile SPIR-V.
//
//go:embed shaders/jpeg420.hlsl
var jpegComputeShader string
