package capture

import (
	"anykvm/internal/framebuf"
	"bytes"
	"errors"
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
