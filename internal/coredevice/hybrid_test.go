package coredevice

import (
	"errors"
	"testing"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
)

type hybridInputFixture struct {
	bridge.HIDSender
	opens, closes, w, h int
	fail                error
}

func (h *hybridInputFixture) Open() error                      { h.opens++; return h.fail }
func (h *hybridInputFixture) Close() error                     { h.closes++; return nil }
func (h *hybridInputFixture) SetInputDimensions(w, height int) { h.w, h.h = w, height }

type hybridVideoFixture struct {
	capture.Device
	opens, closes, requests int
	codec                   string
	stream                  *capture.VideoStreamOptions
	fail                    error
}

func (v *hybridVideoFixture) OpenWithFormat(w, h, fps int) error { v.opens++; return v.fail }
func (v *hybridVideoFixture) Close() error                       { v.closes++; return nil }
func (v *hybridVideoFixture) Capture() (*capture.Frame, error) {
	return &capture.Frame{Width: 1920, Height: 1080, SourceWidth: 1328, SourceHeight: 2896}, nil
}
func (v *hybridVideoFixture) SetVideoCodec(codec string)                          { v.codec = codec }
func (v *hybridVideoFixture) SupportsVideoCodec(codec string) bool                { return codec == "h264" }
func (v *hybridVideoFixture) SetVideoStreamOptions(o *capture.VideoStreamOptions) { v.stream = o }
func (v *hybridVideoFixture) RequestKeyframe() error                              { v.requests++; return nil }
func (v *hybridVideoFixture) SupportsGameStreamHEVC() bool                        { return false }

func TestHybridVideoNegotiationAndFailureCleanup(t *testing.T) {
	i, v := &hybridInputFixture{}, &hybridVideoFixture{}
	d := NewHybrid(v, i)
	d.SetVideoCodec("h264")
	o := &capture.VideoStreamOptions{Codec: "h264", Width: 1920, Height: 1080, FPS: 30}
	d.SetVideoStreamOptions(o)
	if v.codec != "h264" || v.stream != o || !d.SupportsVideoCodec("h264") || d.SupportsGameStreamHEVC() {
		t.Fatal("video settings did not reach video backend")
	}
	if err := d.OpenWithFormat(0, 0, 30); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Capture(); err != nil || i.w != 1328 || i.h != 2896 {
		t.Fatal("HID geometry did not follow source", err, i)
	}
	if err := d.RequestKeyframe(); err != nil || v.requests != 1 {
		t.Fatal("keyframe requested from HID", err)
	}
	if err := d.Close(); err != nil || i.closes != 1 || v.closes != 1 {
		t.Fatal("hybrid close leaked resources")
	}
	v.fail = errors.New("video unavailable")
	inputOpens := i.opens
	if err := d.Open(); !errors.Is(err, v.fail) || i.opens != inputOpens || v.closes != 2 {
		t.Fatal("failed video opened HID or leaked video", err)
	}
	v.fail = nil
	i.fail = errors.New("HID unavailable")
	before := v.opens
	if err := d.Open(); !errors.Is(err, i.fail) || v.opens != before+1 || v.closes != 3 || i.closes != 2 {
		t.Fatal("failed input did not release video and HID", err)
	}
}
