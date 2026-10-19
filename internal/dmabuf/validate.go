package dmabuf

import "fmt"

func (f Frame) Validate() error {
	if f.FD < 0 || f.Width == 0 || f.Height == 0 || f.Width > 16384 || f.Height > 16384 || f.Pitch == 0 ||
		f.CropWidth == 0 || f.CropHeight == 0 || uint64(f.X)+uint64(f.CropWidth) > uint64(f.Width) ||
		uint64(f.Y)+uint64(f.CropHeight) > uint64(f.Height) {
		return fmt.Errorf("invalid DMA-BUF dimensions or crop")
	}
	if f.PlaneCount > 4 {
		return fmt.Errorf("invalid DMA-BUF modifier plane count")
	}
	for i := uint32(0); i < f.PlaneCount; i++ {
		if f.PlanePitches[i] == 0 || (f.Size > 0 && uint64(f.PlaneOffsets[i]) >= f.Size) {
			return fmt.Errorf("invalid DMA-BUF modifier plane layout")
		}
	}
	if f.Modifier == 0 && (uint64(f.Pitch) < uint64(f.Width)*4 || (f.Size > 0 && uint64(f.Offset)+uint64(f.Pitch)*uint64(f.Height-1)+uint64(f.Width)*4 > f.Size)) {
		return fmt.Errorf("linear DMA-BUF is smaller than its visible pixels")
	}
	if f.FourCC != XRGB8888 && f.FourCC != ARGB8888 && f.FourCC != XBGR8888 && f.FourCC != ABGR8888 {
		return fmt.Errorf("unsupported DMA-BUF RGB format 0x%08x", f.FourCC)
	}
	switch f.Rotation {
	case 0, 90, 180, 270:
	default:
		return fmt.Errorf("invalid DMA-BUF rotation")
	}
	return nil
}
