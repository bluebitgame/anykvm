package capture

import (
	"anykvm/internal/framebuf"
	"anykvm/internal/framerate"
	"anykvm/internal/videoenc"
	"testing"
	"time"
	"unsafe"
)

type asyncTestEncoder struct {
	busy, closed bool
	ids          []uint64
	forced       []bool
	packets      []*videoenc.Packet
}

func (e *asyncTestEncoder) Encode([]byte, bool) ([]byte, error) {
	panic("stream used synchronous encoding")
}
func (e *asyncTestEncoder) Name() string             { return "test-async" }
func (e *asyncTestEncoder) Close()                   { e.closed = true }
func (e *asyncTestEncoder) CanSubmit() (bool, error) { return !e.busy, nil }
func (e *asyncTestEncoder) Submit(_ []byte, _, _ int, _ string, id uint64, force bool) (bool, error) {
	if e.busy {
		return false, nil
	}
	e.ids = append(e.ids, id)
	e.forced = append(e.forced, force)
	return true, nil
}
func (e *asyncTestEncoder) Receive() (*videoenc.Packet, error) {
	if len(e.packets) == 0 {
		return nil, nil
	}
	p := e.packets[0]
	e.packets = e.packets[1:]
	return p, nil
}
func (e *asyncTestEncoder) SubmitPixelBuffer(_ unsafe.Pointer, id uint64, force bool) (bool, error) {
	return e.Submit(nil, 0, 0, "", id, force)
}

type testNativeBuffer struct{ released, copied bool }

type testWindowsNativeBuffer struct {
	testNativeBuffer
	adapter videoenc.WindowsAdapter
}

func (b *testWindowsNativeBuffer) WindowsAdapter() videoenc.WindowsAdapter { return b.adapter }

func TestDesktopIdentityReachesEncoderAndCountsOnlyUnencodedDrops(t *testing.T) {
	adapter := videoenc.WindowsAdapter{Set: true, LUID: 0x123456789abcdef0, VendorID: 0x8086}
	e := &asyncTestEncoder{}
	o := videoenc.Options{WindowsAdapter: adapter, Width: 640, Height: 480, FPS: 30, Rate: framerate.New(30, 1), Bitrate: defaultH264BitrateKbps, Codec: "h264"}
	d := &ffmpegDevice{encoder: e, encodeOptions: o, videoCodec: "h264", reqFPS: 30}
	f := &Frame{Width: 640, Height: 480, Format: FormatRaw, CapturedAt: time.Now(), nativeBuffer: &testWindowsNativeBuffer{adapter: adapter}}
	if _, err := d.encodeMediaFrame(f); err != nil || e.closed || len(e.ids) != 1 || d.encodeOptions.WindowsAdapter != adapter {
		t.Fatalf("desktop identity was lost or encoder reinitialized: %v", err)
	}
	f.CapturedAt = f.CapturedAt.Add(time.Millisecond)
	if _, err := d.encodeMediaFrame(f); err != nil || len(e.ids) != 1 || d.skippedEncodeInputs != 1 {
		t.Fatalf("rate limiting submitted stale input or failed to count its drop: %v", err)
	}
}

func (b *testNativeBuffer) Submit(e videoenc.PixelBufferSubmitter, id uint64, force bool) (bool, error) {
	return e.SubmitPixelBuffer(nil, id, force)
}
func (b *testNativeBuffer) Materialize(*Frame) error { b.copied = true; return nil }
func (b *testNativeBuffer) Release()                 { b.released = true }

func TestAsyncEncoderPreservesSourceMetadataAndPendingIDR(t *testing.T) {
	e := &asyncTestEncoder{}
	d := &ffmpegDevice{encoder: e}
	o := videoenc.Options{Width: 640, Height: 480, FPS: 30, Rate: framerate.New(30, 1), Codec: "hevc"}
	first := &Frame{Width: 1280, Height: 720, CapturedAt: time.Now().Add(-time.Second), PresentationPTS: 1000000}
	if out, err := d.submitEncoded(e, o, first, first.CapturedAt); err != nil || out != nil {
		t.Fatalf("premature completion: %v", err)
	}
	e.busy = true
	d.forceIDR.Store(true)
	deadline := d.encodePacer.next
	if _, err := d.submitEncoded(e, o, first, first.CapturedAt); err != nil || !d.forceIDR.Load() || len(d.encodedFrames) != 1 || d.encodePacer.next != deadline {
		t.Fatal("busy submission lost IDR or retained rejected input")
	}
	e.busy = false
	second := &Frame{Width: 1280, Height: 720, CapturedAt: time.Now(), PresentationPTS: 1033333}
	if _, err := d.submitEncoded(e, o, second, second.CapturedAt); err != nil {
		t.Fatal(err)
	}
	if !e.forced[1] {
		t.Fatal("accepted picture did not receive pending IDR request")
	}
	e.packets = []*videoenc.Packet{{ID: e.ids[0], Data: []byte{1}}, {ID: e.ids[1], Data: []byte{2}}}
	for _, src := range []*Frame{first, second} {
		out, err := d.receiveEncoded()
		if err != nil || out == nil || out.CapturedAt != src.CapturedAt || out.PresentationPTS != src.PresentationPTS || out.Width != 640 || out.SourceWidth != 1280 || out.Format != FormatHEVC || out.nativeBuffer != nil {
			t.Fatalf("wrong completion: %+v, %v", out, err)
		}
	}
	if len(d.encodedFrames) != 0 {
		t.Fatal("completion metadata leaked")
	}
}

type nativeTestSource struct {
	Device
	frame *Frame
}

func (s *nativeTestSource) Capture() (*Frame, error) { panic("native path materialized pixels") }
func (s *nativeTestSource) captureNative() (*Frame, error) {
	f := s.frame
	s.frame = nil
	return f, nil
}
func (s *nativeTestSource) Close() error { return nil }

func TestAsyncNativePipeReleasesInputAndDrainsWithoutNewCapture(t *testing.T) {
	e := &asyncTestEncoder{}
	b := &testNativeBuffer{}
	source := &nativeTestSource{frame: &Frame{Width: 320, Height: 240, nativeBuffer: b, CapturedAt: time.Now()}}
	d := newPipeEncoderDevice(source, "h264", 30)
	d.enc.videoCodec = "h264"
	d.enc.reqFPS = 30
	d.enc.encoder = e
	d.enc.encodeOptions = videoenc.Options{Width: 320, Height: 240, FPS: 30, Rate: framerate.New(30, 1), Bitrate: defaultH264BitrateKbps, Codec: "h264"}
	e.busy = true
	if out, err := d.Capture(); err != nil || out != nil || source.frame == nil || b.released {
		t.Fatal("busy encoder consumed the latest native input")
	}
	e.busy = false
	if out, err := d.Capture(); err != nil || out != nil || !b.released || b.copied {
		t.Fatalf("native submission lifecycle: %v", err)
	}
	e.packets = []*videoenc.Packet{{ID: e.ids[0], Data: []byte{1}}}
	if out, err := d.Capture(); err != nil || out == nil || out.nativeBuffer != nil {
		t.Fatalf("idle completion: %+v, %v", out, err)
	}
	if err := d.Close(); err != nil || !e.closed || len(d.enc.encodedFrames) != 0 {
		t.Fatal("close did not release encoder state")
	}
	if _, err := d.Capture(); err != ErrStreamStopped {
		t.Fatal("capture restarted after close")
	}
}

func TestIOSUSBNativeEncodingPreservesDimensionsAndOrientation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, size := range [][3]int{{1206, 2622, 0}, {2622, 1206, 270}} {
			e := &asyncTestEncoder{}
			b := &testNativeBuffer{}
			at := time.Now()
			input := &Frame{Format: FormatYUV, PixFmt: "nv12", Width: size[0], Height: size[1], Rotation: size[2], CapturedAt: at, nativeBuffer: b}
			o := videoenc.Options{Width: size[0], Height: size[1], FPS: 30, Rate: framerate.New(30, 1), Bitrate: defaultH264BitrateKbps, Codec: "h264"}
			d := &ffmpegDevice{encoder: e, encodeOptions: o, videoCodec: "h264", reqFPS: 30, targetW: 1920, targetH: 1080, variableInputRate: true}
			if stream {
				d.streamOptions = &VideoStreamOptions{Width: 1920, Height: 1080, FPS: 30, Bitrate: o.Bitrate, Codec: "h264"}
			}
			if out, err := d.encodeMediaFrame(input); err != nil || out != nil || len(e.ids) != 1 || b.copied {
				t.Fatalf("stream=%t size=%v: native submission copied or resized: %v", stream, size, err)
			}
			e.packets = []*videoenc.Packet{{ID: e.ids[0], Data: []byte{1}}}
			out, err := d.receiveEncoded()
			if err != nil || out == nil || out.Width != size[0] || out.Height != size[1] || out.SourceWidth != size[0] || out.SourceHeight != size[1] || out.Rotation != size[2] || out.CapturedAt != at || out.nativeBuffer != nil {
				t.Fatalf("stream=%t size=%v: source geometry lost: %+v, %v", stream, size, out, err)
			}
			out.Release()
		}
	}
}

func TestAsyncOutputOwnershipTransferAndDiscard(t *testing.T) {
	freed := 0
	packet := func(id uint64, dropped bool) *videoenc.Packet {
		data := []byte{1, 2, 3}
		return &videoenc.Packet{ID: id, Dropped: dropped, Data: data, Buffer: framebuf.New(data, func() { freed++ })}
	}
	unknown := packet(99, false)
	e := &asyncTestEncoder{packets: []*videoenc.Packet{unknown}}
	d := &ffmpegDevice{encoder: e, encodedFrames: map[uint64]*Frame{1: {}, 2: {}}}
	if _, err := d.receiveEncoded(); err == nil || freed != 1 {
		t.Fatal("unknown completion retained native output")
	}
	dropped, ready := packet(1, true), packet(2, false)
	owner := ready.Buffer
	e.packets = []*videoenc.Packet{dropped, ready}
	f, err := d.receiveEncoded()
	if err != nil || f == nil || f.Buffer != owner || freed != 2 || ready.Buffer != nil {
		t.Fatal("completion ownership was not transferred")
	}
	f.Release()
	if freed != 3 {
		t.Fatal("completed frame leaked")
	}
}
