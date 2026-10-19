package capture

import (
	"anykvm/internal/framerate"
	"anykvm/internal/videoenc"
	"bytes"
	"errors"
	"image/jpeg"
	"testing"
	"time"
)

type textureFixture struct {
	handled, materialized, forced bool
	err                           error
}

func (n *textureFixture) Submit(videoenc.PixelBufferSubmitter, uint64, bool) (bool, error) {
	return false, errors.New("unexpected async submission")
}
func (n *textureFixture) Release() {}
func (n *textureFixture) EncodeTexture(_ videoenc.Encoder, force bool) ([]byte, bool, error) {
	n.forced = force
	if !n.handled {
		return nil, false, n.err
	}
	return []byte{0, 0, 0, 1, 0x65, 1}, true, n.err
}
func (n *textureFixture) Materialize(f *Frame) error {
	n.materialized = true
	f.Data = make([]byte, f.Width*f.Height*4)
	f.nativeBuffer = nil
	for i := 3; i < len(f.Data); i += 4 {
		f.Data[i] = 255
	}
	return nil
}

type textureEncoderFixture struct {
	calls int
	force bool
}

func (*textureEncoderFixture) Name() string { return "texture test" }
func (*textureEncoderFixture) Close()       {}
func (e *textureEncoderFixture) Encode(_ []byte, force bool) ([]byte, error) {
	e.calls++
	e.force = force
	return []byte{0, 0, 0, 1, 0x65, 2}, nil
}
func TestNativeTextureEncodingAvoidsReadback(t *testing.T) {
	for _, handled := range []bool{true, false} {
		t.Run(map[bool]string{true: "GPU", false: "CPU fallback"}[handled], func(t *testing.T) {
			o := videoenc.Options{Width: 4, Height: 4, FPS: 30, Rate: framerate.New(30, 1), Bitrate: 12000, Codec: "h264"}
			e := &textureEncoderFixture{}
			d := &ffmpegDevice{videoCodec: "h264", reqFPS: 30, encoder: e, encodeOptions: o}
			d.forceIDR.Store(true)
			n := &textureFixture{handled: handled}
			now := time.Now()
			f := &Frame{Width: 4, Height: 4, Format: FormatRaw, CapturedAt: now, nativeBuffer: n}
			out, err := d.encodeMediaFrame(f)
			if err != nil {
				t.Fatal(err)
			}
			if out == nil {
				t.Fatal("missing encoded frame")
			}
			defer out.Release()
			if out.Format != FormatH264 || out.CapturedAt != now || len(out.Data) == 0 {
				t.Fatalf("invalid frame: %+v", out)
			}
			if !n.forced {
				t.Fatal("GPU path did not receive IDR request")
			}
			if handled && (n.materialized || e.calls != 0) {
				t.Fatal("GPU path read back or re-encoded desktop pixels")
			}
			if !handled && (!n.materialized || e.calls != 1 || !e.force) {
				t.Fatal("fallback lost pixels or IDR request")
			}
		})
	}
}
func TestNativeTextureErrorDoesNotSilentlyReadBack(t *testing.T) {
	o := videoenc.Options{Width: 4, Height: 4, FPS: 30, Rate: framerate.New(30, 1), Bitrate: 12000, Codec: "h264"}
	expected := errors.New("driver failed")
	n := &textureFixture{handled: true, err: expected}
	d := &ffmpegDevice{videoCodec: "h264", reqFPS: 30, encoder: &textureEncoderFixture{}, encodeOptions: o}
	_, err := d.encodeMediaFrame(&Frame{Width: 4, Height: 4, Format: FormatRaw, nativeBuffer: n})
	if !errors.Is(err, expected) || n.materialized {
		t.Fatalf("error=%v readback=%v", err, n.materialized)
	}
}
func TestNativeReadbackForJPEGAndRaw(t *testing.T) {
	for _, decode := range []bool{true, false} {
		t.Run(map[bool]string{true: "raw", false: "JPEG"}[decode], func(t *testing.T) {
			n := &textureFixture{}
			d := &ffmpegDevice{decodeImage: decode}
			f, err := d.encodeMediaFrame(&Frame{Width: 4, Height: 4, Format: FormatRaw, nativeBuffer: n})
			if err != nil {
				t.Fatal(err)
			}
			defer f.Release()
			if !n.materialized {
				t.Fatal("pixel consumer did not read back native pixels")
			}
			if decode {
				if f.Format != FormatRaw || len(f.Data) != 64 {
					t.Fatal("invalid raw pixels")
				}
			} else {
				if f.Format != FormatJPEG {
					t.Fatal("missing JPEG")
				}
				if _, err := jpeg.Decode(bytes.NewReader(f.Data)); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
