//go:build linux && cgo

package rdp

import _ "embed"

//go:embed shaders/progressive.spv
var progressiveTransformSPV []byte

//go:embed shaders/progressive_encodeTiles.spv
var progressiveEntropySPV []byte

//go:embed shaders/progressive_indexSegments.spv
var progressiveIndexSPV []byte

//go:embed shaders/progressive_packSegments.spv
var progressivePackSPV []byte

//go:embed shaders/progressive_surface.spv
var progressiveSurfaceSPV []byte

func progressiveSurfaceSPIRV() []byte { return progressiveSurfaceSPV }

func progressiveSPIRV() [4][]byte {
	return [4][]byte{progressiveTransformSPV, progressiveEntropySPV, progressiveIndexSPV, progressivePackSPV}
}
