//go:build cgo

package gamestream

import (
	"bytes"
	"image"
	"image/jpeg"
	"testing"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"anykvm/internal/videoenc"
)

type hevcFixtureEncoder struct{}

func (*hevcFixtureEncoder) Name() string { return "HEVC fixture" }
func (*hevcFixtureEncoder) Close()       {}
func (*hevcFixtureEncoder) Encode(_ []byte, force bool) ([]byte, error) {
	if force {
		return hevcNALs(32, 33, 34, 19), nil
	}
	return hevcNALs(1), nil
}

// Exercise the dispatch through videoLoop, conversion and real UDP packetization.
// HEVC used to reject raw/JPEG input before ever opening the output encoder.
func TestHEVCRawAndJPEGVideoLoop(t *testing.T) {
	for _, format := range []capture.VideoFormat{capture.FormatRaw, capture.FormatJPEG} {
		name := "raw"
		if format == capture.FormatJPEG {
			name = "jpeg"
		}
		t.Run(name, func(t *testing.T) {
			x, peer, cancel, _ := passiveVideoSession(t)
			x.options.Codec = "hevc"
			x.options.Width, x.options.Height = 2, 2
			data := make([]byte, 2*2*4)
			if format == capture.FormatJPEG {
				var output bytes.Buffer
				if err := jpeg.Encode(&output, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
					t.Fatal(err)
				}
				data = output.Bytes()
			}
			x.frames <- bridge.Frame{Format: format, Width: 2, Height: 2, Data: data}
			opened := make(chan videoenc.Options, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				x.videoLoopUsing(func(o videoenc.Options) (videoenc.Encoder, error) {
					opened <- o
					return &hevcFixtureEncoder{}, nil
				})
			}()
			defer func() { cancel(); <-done }()
			first, _, _ := receivePassivePicture(t, peer, x.options.PacketSize)
			if !isRecoveryFrame(first, capture.FormatHEVC) {
				t.Fatalf("missing HEVC recovery picture: %x", first)
			}
			o := <-opened
			if o.Codec != "hevc" || o.Width != 2 || o.Height != 2 {
				t.Fatal(o)
			}
			second, _, _ := receivePassivePicture(t, peer, x.options.PacketSize)
			if !bytes.Equal(second, hevcNALs(1)) {
				t.Fatalf("missing predicted picture: %x", second)
			}
		})
	}
}
