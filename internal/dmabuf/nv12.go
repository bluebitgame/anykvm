package dmabuf

import "fmt"

// ValidateNV12 validates a linear two-plane image in one borrowed DMA-BUF.
// RGB modifier layouts continue to use Validate.
func (f Frame) ValidateNV12() error {
	if f.FD < 0 || f.FourCC != NV12 || f.Width < 2 || f.Height < 2 || f.Width > 8192 || f.Height > 8192 || (f.Width|f.Height)&1 != 0 ||
		f.Modifier != 0 || f.PlaneCount != 2 || f.Pitch < f.Width || f.PlanePitches[0] != f.Pitch || f.PlanePitches[1] != f.Pitch ||
		f.X != 0 || f.Y != 0 || f.CropWidth != f.Width || f.CropHeight != f.Height || f.Rotation != 0 {
		return fmt.Errorf("unsupported linear NV12 DMA-BUF layout")
	}
	yend := uint64(f.PlaneOffsets[0]) + uint64(f.Pitch)*uint64(f.Height)
	uvend := uint64(f.PlaneOffsets[1]) + uint64(f.Pitch)*uint64(f.Height/2)
	if uint64(f.PlaneOffsets[1]) < yend || f.Size == 0 || uvend > f.Size {
		return fmt.Errorf("NV12 DMA-BUF planes exceed allocation")
	}
	return nil
}
