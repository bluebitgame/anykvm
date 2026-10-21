package capture

import (
	"anykvm/internal/framebuf"
	"anykvm/internal/jpegcodec"
	"bytes"
	"errors"
	"os"
	"testing"
	"time"
)

type jpegTextureFixture struct {
	testNativeBuffer
	width, height, quality int
	calls                  int
	handled                bool
	err                    error
}

func (*jpegTextureFixture) JPEGEncoderName() string { return "test GPU JPEG" }

func (f *jpegTextureFixture) EncodeJPEG(w, h, q int) (*framebuf.Buffer, bool, error) {
	f.width, f.height, f.quality = w, h, q
	f.calls++
	if f.err != nil || !f.handled {
		return nil, false, f.err
	}
	return framebuf.New([]byte{255, 216, 255, 217}, nil), true, nil
}

func (f *jpegTextureFixture) Materialize(out *Frame) error {
	f.copied = true
	out.Data = bytes.Repeat([]byte{15, 80, 190, 255}, out.Width*out.Height)
	out.nativeBuffer = nil
	return nil
}

func TestJPEGTextureBeforeReadback(t *testing.T) {
	d := &ffmpegDevice{videoCodec: "mjpeg", targetW: 32, targetH: 32, quality: 91, scalableInput: true}
	native := &jpegTextureFixture{handled: true}
	at := time.Now()
	f := &Frame{Format: FormatRaw, Width: 64, Height: 32, CapturedAt: at, nativeBuffer: native}
	out, err := d.encodeMediaFrame(f)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Release()
	if native.copied || native.calls != 1 || native.width != 32 || native.height != 16 || native.quality != 91 {
		t.Fatalf("texture path did not preserve scaling/quality: %+v", native)
	}
	if out.Format != FormatJPEG || out.Width != 32 || out.Height != 16 || out.CapturedAt != at || out.Buffer == nil || out.nativeBuffer != nil {
		t.Fatalf("invalid output metadata: %+v", out)
	}
}

func TestJPEGTextureFallbackAndDecodePolicy(t *testing.T) {
	for _, decode := range []bool{false, true} {
		d := &ffmpegDevice{videoCodec: "mjpeg", decodeImage: decode}
		native := &jpegTextureFixture{}
		out, err := d.encodeMediaFrame(&Frame{Format: FormatRaw, Width: 16, Height: 16, nativeBuffer: native})
		if err != nil {
			t.Fatal(err)
		}
		out.Release()
		if !native.copied || (decode && native.calls != 0) || (!decode && native.calls != 1) {
			t.Fatalf("decode=%v: %+v", decode, native)
		}
		if decode && out.Format != FormatRaw {
			t.Fatal("raw consumer received JPEG")
		}
	}
	d := &ffmpegDevice{videoCodec: "mjpeg"}
	failure := errors.New("texture failed")
	native := &jpegTextureFixture{err: failure}
	if _, err := d.encodeMediaFrame(&Frame{Format: FormatRaw, Width: 16, Height: 16, nativeBuffer: native}); !errors.Is(err, failure) {
		t.Fatalf("lost error: %v", err)
	}
}

type jpegRegionTextureFixture struct {
	jpegTextureFixture
	regions     *jpegcodec.RestartJPEG
	regionCalls int
}

func (f *jpegRegionTextureFixture) EncodeJPEGRegions(w, h, q int) (*jpegcodec.RestartJPEG, bool, error) {
	f.regionCalls++
	f.width, f.height, f.quality = w, h, q
	return f.regions, f.handled, f.err
}
func TestJPEGTextureRegionReadbackPolicy(t *testing.T) {
	data, err := os.ReadFile("../jpegcodec/testdata/restart_base.jpeg")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := jpegcodec.ParseRestartJPEG(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, regions := range []bool{false, true} {
		for _, decode := range []bool{false, true} {
			d := &ffmpegDevice{videoCodec: "mjpeg", jpegRegions: regions, decodeImage: decode, quality: 85}
			native := &jpegRegionTextureFixture{jpegTextureFixture: jpegTextureFixture{handled: true}, regions: snapshot}
			at := time.Now()
			out, err := d.encodeMediaFrame(&Frame{Format: FormatRaw, Width: 193, Height: 131, CapturedAt: at, nativeBuffer: native})
			if err != nil {
				t.Fatal(err)
			}
			defer out.Release()
			if regions && !decode {
				if native.regionCalls != 1 || native.calls != 0 || native.copied || out.JPEGRegions != snapshot || len(out.Data) != 0 || out.Buffer != nil || out.CapturedAt != at {
					t.Fatalf("compact GPU path not selected: %+v", out)
				}
			} else if native.regionCalls != 0 || out.JPEGRegions != nil {
				t.Fatal("compact GPU path selected for full JPEG/raw consumer")
			}
		}
	}
}
