// Package dmabuf describes borrowed Linux DMA-BUF images without a graphics API dependency.
package dmabuf

// Frame is one RGB or NV12 image in a single allocation. FD remains owned by the producer throughout a
// synchronous consumer call. Consumers which retain it must duplicate the FD.
// Modifier, offset and pitch must be preserved; tiled buffers cannot be mmap'd
// or interpreted as a linear byte array.
type Frame struct {
	// Auxiliary DRM modifier planes may share the same memory object. A zero
	// PlaneCount uses the single packed plane described by Pitch and Offset.
	PlaneCount                           uint32
	PlanePitches, PlaneOffsets           [4]uint32
	FD                                   int
	Width, Height, FourCC, Pitch, Offset uint32
	Modifier, Size                       uint64
	X, Y, CropWidth, CropHeight          uint32
	FlipY                                bool   // flip source vertically before display rotation
	Rotation                             int    // clockwise degrees
	DeviceMajor, DeviceMinor             uint32 // DRM primary device of the source
}

const (
	XRGB8888 = uint32('X') | uint32('R')<<8 | uint32('2')<<16 | uint32('4')<<24
	ARGB8888 = uint32('A') | uint32('R')<<8 | uint32('2')<<16 | uint32('4')<<24
	XBGR8888 = uint32('X') | uint32('B')<<8 | uint32('2')<<16 | uint32('4')<<24
	ABGR8888 = uint32('A') | uint32('B')<<8 | uint32('2')<<16 | uint32('4')<<24
	NV12     = uint32('N') | uint32('V')<<8 | uint32('1')<<16 | uint32('2')<<24
)
