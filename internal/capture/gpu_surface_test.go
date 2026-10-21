package capture

import (
	"anykvm/internal/framebuf"
	"anykvm/internal/gpuconv"
	"errors"
	"testing"
)

type surfaceFixture struct {
	testNativeBuffer
	calls   int
	surface *gpuconv.Surface
	owner   *framebuf.Buffer
	err     error
}

func (f *surfaceFixture) SnapshotSurface() (*gpuconv.Surface, *framebuf.Buffer, bool, error) {
	f.calls++
	return f.surface, f.owner, true, f.err
}

func TestGPUSurfaceBeforeReadback(t *testing.T) {
	for _, decode := range []bool{false, true} {
		d := &ffmpegDevice{videoCodec: "rawvideo", gpuSurface: true, decodeImage: decode}
		freed := 0
		s := &surfaceFixture{surface: &gpuconv.Surface{Width: 17, Height: 9, SharedTexture: 1}, owner: framebuf.New(nil, func() { freed++ })}
		out, err := d.encodeMediaFrame(&Frame{Width: 17, Height: 9, Format: FormatRaw, nativeBuffer: s})
		if err != nil {
			t.Fatal(err)
		}
		if out == nil || out.Surface != s.surface || out.Buffer == nil || out.Format != FormatSurface || out.nativeBuffer != nil || len(out.Data) != 0 || s.copied || s.calls != 1 {
			t.Fatal("GPU frame was materialized or lost its snapshot")
		}
		out.Release()
		if freed != 1 {
			t.Fatal("snapshot not released")
		}
	}
}
func TestGPUSurfacePoolAndFailure(t *testing.T) {
	d := &ffmpegDevice{videoCodec: "rawvideo", gpuSurface: true}
	s := &surfaceFixture{}
	out, err := d.encodeMediaFrame(&Frame{Width: 16, Height: 16, nativeBuffer: s})
	if err != nil || out != nil || s.copied {
		t.Fatal("full snapshot pool must drop without readback")
	}
	failure := errors.New("GPU copy failed")
	freed := 0
	s = &surfaceFixture{err: failure, owner: framebuf.New(nil, func() { freed++ })}
	if _, err = d.encodeMediaFrame(&Frame{Width: 16, Height: 16, nativeBuffer: s}); !errors.Is(err, failure) || s.copied || freed != 1 {
		t.Fatal("GPU failure caused readback or leaked its owner")
	}
}
func TestGPUSurfaceRequiresOptIn(t *testing.T) {
	d := &ffmpegDevice{videoCodec: "rawvideo"}
	s := &surfaceFixture{}
	out, err := d.encodeMediaFrame(&Frame{Width: 4, Height: 4, nativeBuffer: s})
	if err != nil {
		t.Fatal(err)
	}
	out.Release()
	if s.calls != 0 || !s.copied {
		t.Fatal("ordinary raw capture changed to GPU surfaces")
	}
}
