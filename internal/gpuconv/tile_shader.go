package gpuconv

import (
	_ "embed"
	"strings"
)

//go:embed shaders/tile_compare.hlsl
var tileCompareShader string

// WithTileCompare expands the shared kernel before runtime D3D compilation.
// tools/build_shaders.py performs the same expansion for Vulkan and provenance.
func WithTileCompare(source, entry string) string {
	kernel := strings.Replace(tileCompareShader, "void compareTile(", "void "+entry+"(", 1)
	return strings.ReplaceAll(source, "// ANYKVM_TILE_COMPARE", kernel)
}
