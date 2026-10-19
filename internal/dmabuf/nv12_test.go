package dmabuf

import "testing"

func TestLinearNV12Planes(t *testing.T) {
	f := Frame{FD: 0, Width: 1920, Height: 1080, FourCC: NV12, Pitch: 2048, Size: 2048 * 1080 * 3 / 2, PlaneCount: 2,
		PlanePitches: [4]uint32{2048, 2048}, PlaneOffsets: [4]uint32{0, 2048 * 1080}, CropWidth: 1920, CropHeight: 1080}
	if err := f.ValidateNV12(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Frame){
		"truncated": func(x *Frame) { x.Size-- }, "overlap": func(x *Frame) { x.PlaneOffsets[1]-- },
		"odd size": func(x *Frame) { x.Height-- }, "modifier": func(x *Frame) { x.Modifier = 1 },
		"short stride": func(x *Frame) { x.Pitch = 100 }, "chroma stride": func(x *Frame) { x.PlanePitches[1] = 1024 },
		"invalid fd": func(x *Frame) { x.FD = -1 }, "crop": func(x *Frame) { x.X = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := f
			mutate(&bad)
			if bad.ValidateNV12() == nil {
				t.Fatal("unsafe NV12 layout accepted")
			}
		})
	}
	if f.Validate() == nil {
		t.Fatal("NV12 accepted by the RGB conversion path")
	}
}
