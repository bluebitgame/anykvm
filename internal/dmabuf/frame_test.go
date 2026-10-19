package dmabuf

import "testing"

func TestRejectUnsafeDMABUF(t *testing.T) {
	valid := Frame{FD: 10, Width: 1920, Height: 1080, FourCC: XRGB8888, Pitch: 7680, CropWidth: 1920, CropHeight: 1080}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Frame){
		"closed": func(f *Frame) { f.FD = -1 }, "rotation": func(f *Frame) { f.Rotation = 45 },
		"crop overflow": func(f *Frame) { f.X = 0xffffffff }, "empty crop": func(f *Frame) { f.CropHeight = 0 },
		"YUV": func(f *Frame) { f.FourCC = NV12 }, "stride": func(f *Frame) { f.Pitch = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := valid
			mutate(&bad)
			if bad.Validate() == nil {
				t.Fatal("unsafe descriptor accepted")
			}
		})
	}
}
