//go:build cgo

package gamestream

import (
	"context"
	"errors"
	"testing"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"anykvm/internal/videoenc"
)

type qpcFixtureEncoder struct{ codec string }

func (*qpcFixtureEncoder) Name() string { return "QPC fixture" }
func (*qpcFixtureEncoder) Close()       {}
func (e *qpcFixtureEncoder) Encode(_ []byte, _ bool) ([]byte, error) {
	if e.codec == "hevc" {
		return hevcNALs(32, 33, 34, 19), nil
	}
	return []byte{0, 0, 0, 1, 0x65, 1}, nil
}

func TestQPCPlaybackOnDirectAndReencodedWire(t *testing.T) {
	for _, codec := range []string{"h264", "hevc"} {
		for _, direct := range []bool{true, false} {
			t.Run(codec+map[bool]string{true: "/direct", false: "/encoded"}[direct], func(t *testing.T) {
				x, peer, cancel, _ := passiveVideoSession(t)
				x.options.Codec, x.options.Width, x.options.Height = codec, 2, 2
				frame := func(i int) bridge.Frame {
					f := bridge.Frame{Format: capture.FormatRaw, PreserveSource: true, Width: 2, Height: 2,
						Data: make([]byte, 16), PresentationPTS: 123456789 + int64(i)*16667,
						CapturedAt: time.Now().Add(time.Duration(i-1) * time.Hour)}
					if direct {
						f.Format = capture.FormatH264
						if codec == "hevc" {
							f.Format = capture.FormatHEVC
						}
						f.Data, _ = (&qpcFixtureEncoder{codec}).Encode(nil, true)
					}
					return f
				}
				done := make(chan error, 1)
				go func() {
					v := &videoPacketizer{}
					var err error
					if direct {
						_, err = x.streamEncodedVideo(frame(0), v, time.Now())
					} else {
						_, err = x.encodeVideoUsing(frame(0), v, time.Now(), func(videoenc.Options) (videoenc.Encoder, error) { return &qpcFixtureEncoder{codec}, nil })
					}
					done <- err
				}()
				defer func() {
					cancel()
					if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
						t.Error(err)
					}
				}()
				for i := 0; i < 3; i++ {
					if i > 0 {
						x.server.PushFrame(frame(i))
					}
					_, pts, index := receivePassivePicture(t, peer, x.options.PacketSize)
					if pts != 90+uint32(i*1500) || index != uint32(i+1) {
						t.Fatalf("wire PTS=%d index=%d, picture=%d", pts, index, i)
					}
				}
			})
		}
	}
}
