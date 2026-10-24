package capture

import (
	"bytes"
	"testing"
	"time"

	"anykvm/internal/dmabuf"
	"anykvm/internal/framebuf"
	"anykvm/internal/framerate"
	"anykvm/internal/gpuconv"
	"anykvm/internal/jpegcodec"
	"anykvm/internal/videoenc"
)

type dmaVideoFixture struct {
	textureEncoderFixture
	source  dmabuf.Frame
	key     bool
	repeats int
}

func (e *dmaVideoFixture) EncodeDMABUF(source dmabuf.Frame, key bool) ([]byte, bool, error) {
	e.source, e.key = source, key
	return []byte{1, 2, 3}, true, nil
}
func (e *dmaVideoFixture) EncodeLast(key bool) ([]byte, error) {
	e.key = key
	e.repeats++
	return []byte{4, 5, 6}, nil
}

func TestDMABUFNegotiatedVideoAndIdleRefresh(t *testing.T) {
	for _, codec := range []string{"h264", "hevc", "av1"} {
		t.Run(codec, func(t *testing.T) {
			d := NewDMABUFEncoder(DMABUFEncoderOptions{RequireGPUEncoding: true, IdleRefresh: true})
			d.SetVideoCodec(codec)
			d.Configure(320, 240, 30)
			e := &dmaVideoFixture{}
			d.encoder = e
			d.encodeOptions = videoenc.Options{Width: 320, Height: 240, FPS: 30, Rate: framerate.New(30, 1), Bitrate: 12000, Codec: codec, DRMDeviceMajor: 226, DRMDeviceMinor: 128}
			defer d.Close()
			source := dmabuf.Frame{FD: 7, Width: 640, Height: 480, Pitch: 2560, CropWidth: 640, CropHeight: 480, FourCC: dmabuf.XRGB8888, DeviceMajor: 226, DeviceMinor: 128, FlipY: true}
			at := time.Now()
			out, err := d.EncodeSource(source, 640, 480, at, 12345)
			if err != nil || out == nil {
				t.Fatalf("output: %+v %v", out, err)
			}
			defer out.Release()
			if out.Format.String() != codec || out.Width != 320 || out.Height != 240 || out.SourceWidth != 640 || out.SourceHeight != 480 || out.PresentationPTS != 12345 || out.CapturedAt != at || out.nativeBuffer != nil {
				t.Fatalf("lost negotiated format/geometry/time: %+v", out)
			}
			if e.source != source || !e.key || e.calls != 0 || d.Stats().CPUReadbacks != 0 || d.Stats().GPUFrames != 1 {
				t.Fatal("did not encode directly from borrowed DMA-BUF")
			}
			repeated, err := d.Repeat()
			if err != nil || repeated == nil {
				t.Fatal("idle refresh:", err)
			}
			defer repeated.Release()
			if repeated.Format != out.Format || !bytes.Equal(repeated.Data, []byte{4, 5, 6}) || repeated.PresentationPTS != 0 || e.repeats != 1 || !e.key || e.calls != 0 {
				t.Fatal("idle refresh reused source storage or lost codec/IDR")
			}
			if !bytes.Equal(out.Data, []byte{1, 2, 3}) {
				t.Fatal("refresh mutated outstanding encoded frame")
			}
		})
	}
}

type dmaJPEGFixture struct{ regions bool }

func (*dmaJPEGFixture) DRMDevice() (uint32, uint32) { return 226, 128 }
func (*dmaJPEGFixture) Close()                      {}
func (*dmaJPEGFixture) EncodeSource(dmabuf.Frame, int, int, int) ([]byte, error) {
	return []byte{0xff, 0xd8, 1, 0xff, 0xd9}, nil
}
func (e *dmaJPEGFixture) EncodeRegions(dmabuf.Frame, int, int, int) (*jpegcodec.RestartJPEG, error) {
	e.regions = true
	return &jpegcodec.RestartJPEG{}, nil
}

func TestDMABUFJPEGUsesCommonRoutingAndOwnedIdleCache(t *testing.T) {
	t.Setenv("ANYKVM_LINUX_GPU_JPEG", "1")
	for _, regions := range []bool{false, true} {
		t.Run(map[bool]string{false: "jpeg", true: "regions"}[regions], func(t *testing.T) {
			d := NewDMABUFEncoder(DMABUFEncoderOptions{RequireGPUEncoding: true, IdleRefresh: true})
			d.Configure(0, 0, 30)
			d.SetDecodeImage(false)
			d.SetJPEGRegions(regions)
			d.jpegEncoder = &dmaJPEGFixture{}
			source := dmabuf.Frame{FD: 7, Width: 640, Height: 480, Pitch: 2560, CropWidth: 640, CropHeight: 480, FourCC: dmabuf.XRGB8888}
			out, err := d.EncodeSource(source, 640, 480, time.Now(), 0)
			if err != nil || out == nil {
				t.Fatal("JPEG:", err)
			}
			if out.Format != FormatJPEG || (out.JPEGRegions != nil) != regions || d.readbacks != 0 || d.gpuFrames != 1 || out.nativeBuffer != nil {
				t.Fatalf("wrong JPEG route: %+v", out)
			}
			data := bytes.Clone(out.Data)
			out.Release()
			repeated, err := d.Repeat()
			if err != nil || repeated == nil || !bytes.Equal(repeated.Data, data) || (repeated.JPEGRegions != nil) != regions {
				t.Fatal("cache lost released JPEG output", err)
			}
			d.Close()
			if !bytes.Equal(repeated.Data, data) {
				t.Fatal("close freed an outstanding refresh")
			}
			repeated.Release()
		})
	}
}

func TestDMABUFEncodingFailureCannotReadBackPixels(t *testing.T) {
	d := NewDMABUFEncoder(DMABUFEncoderOptions{RequireGPUEncoding: true})
	defer d.Close()
	source := dmabuf.Frame{FD: 7, Width: 640, Height: 480, Pitch: 2560, CropWidth: 640, CropHeight: 480, FourCC: dmabuf.XRGB8888}
	native := &dmabufFrame{encoder: d, source: source, width: 640, height: 480}
	if _, handled, err := native.EncodeTexture(&textureEncoderFixture{}, true); err == nil || !handled {
		t.Fatal("non-DMA-BUF video encoder was accepted")
	}
	if err := native.MaterializeVideo(&Frame{Width: 640, Height: 480}); err == nil {
		t.Fatal("video fallback allowed CPU readback")
	}
	d.SetVideoCodec("av1")
	if err := native.Materialize(&Frame{Width: 640, Height: 480}); err == nil {
		t.Fatal("async video fallback allowed CPU readback")
	}
	d.SetVideoCodec("mjpeg")
	t.Setenv("ANYKVM_LINUX_GPU_JPEG", "0")
	if _, handled, err := native.EncodeJPEG(640, 480, 80); err == nil || !handled {
		t.Fatal("disabled GPU JPEG allowed CPU fallback")
	}
	if d.converter != nil || d.readbacks != 0 {
		t.Fatal("failure opened a pixel readback path")
	}
}

type dmaSurfaceFixture struct {
	textureFixture
	freed *int
}

func (n *dmaSurfaceFixture) SnapshotSurface() (*gpuconv.Surface, *framebuf.Buffer, bool, error) {
	return &gpuconv.Surface{Width: 4, Height: 4, SharedTexture: 1}, framebuf.New(nil, func() { *n.freed = *n.freed + 1 }), true, nil
}

func TestDMABUFSurfaceCacheKeepsOnlyOwnedSnapshot(t *testing.T) {
	d := NewDMABUFEncoder(DMABUFEncoderOptions{RequireGPUEncoding: true, IdleRefresh: true})
	d.Configure(0, 0, 30)
	d.SetVideoCodec("rawvideo")
	d.SetGPUSurface(true)
	freed := 0
	native := &dmaSurfaceFixture{freed: &freed}
	out, err := d.encodeFrame(&Frame{Width: 4, Height: 4, nativeBuffer: native})
	if err != nil || out == nil || out.Format != FormatSurface || out.nativeBuffer != nil || native.materialized {
		t.Fatal("surface snapshot route", err)
	}
	out.Release()
	refresh, err := d.Repeat()
	if err != nil || refresh == nil || freed != 0 {
		t.Fatal("surface cache lost ownership", err)
	}
	d.Close()
	if freed != 0 {
		t.Fatal("close freed surface held by downstream")
	}
	refresh.Release()
	if freed != 1 {
		t.Fatal("GPU snapshot release count:", freed)
	}
}

func TestDMABUFInvalidInputDoesNotReachEncoder(t *testing.T) {
	d := NewDMABUFEncoder(DMABUFEncoderOptions{RequireGPUEncoding: true})
	defer d.Close()
	if _, err := d.EncodeSource(dmabuf.Frame{FD: -1}, 0, 0, time.Now(), 0); err == nil {
		t.Fatal("missing source validation", err)
	}
	if d.encoder != nil || d.converter != nil || d.gpuFrames != 0 {
		t.Fatal("invalid source reached GPU")
	}
}

func TestDMABUFGameStreamOptionsTakePrecedence(t *testing.T) {
	d := NewDMABUFEncoder(DMABUFEncoderOptions{RequireGPUEncoding: true, IdleRefresh: true})
	d.Configure(0, 0, 30)
	d.SetVideoCodec("h264")
	d.SetVideoBitrate(4000)
	d.SetVideoStreamOptions(&VideoStreamOptions{Codec: "hevc", Width: 1280, Height: 720, FPS: 60, Bitrate: 16000})
	e := &dmaVideoFixture{}
	d.encoder = e
	d.encodeOptions = videoenc.Options{Codec: "hevc", Width: 1280, Height: 720, FPS: 60, Rate: framerate.New(60, 1), Bitrate: 16000, DRMDeviceMajor: 226, DRMDeviceMinor: 128}
	defer d.Close()
	source := dmabuf.Frame{FD: 7, Width: 640, Height: 480, Pitch: 2560, CropWidth: 640, CropHeight: 480, FourCC: dmabuf.XRGB8888, DeviceMajor: 226, DeviceMinor: 128}
	out, err := d.EncodeSource(source, 640, 480, time.Now(), 0)
	if err != nil || out == nil {
		t.Fatalf("negotiated stream: %+v %v", out, err)
	}
	defer out.Release()
	if out.Format != FormatHEVC || out.Width != 1280 || out.Height != 720 || !e.key || e.calls != 0 || d.readbacks != 0 {
		t.Fatal("stream options lost to source/default encoder settings")
	}
}

func TestDMABUFOptionalCodecCapabilitiesMatchFallbackPolicy(t *testing.T) {
	// A library-only declaration must not enable optional video for strict SPICE.
	// Conversely, a working Vulkan path must remain selectable in an ffmpeg build.
	for _, codec := range []string{"hevc", "av1"} {
		for _, requireGPU := range []bool{false, true} {
			for _, gpuAvailable := range []bool{false, true} {
				for _, pixelsAvailable := range []bool{false, true} {
					d := NewDMABUFEncoder(DMABUFEncoderOptions{RequireGPUEncoding: requireGPU})
					got := d.supportsVideoCodec(codec, func(string) bool { return gpuAvailable }, func(string) bool {
						if requireGPU {
							t.Fatal("strict DMA-BUF negotiation consulted a pixel encoder")
						}
						return pixelsAvailable
					})
					want := gpuAvailable || !requireGPU && pixelsAvailable
					if got != want {
						t.Fatalf("codec=%s strict=%t Vulkan=%t pixels=%t: got %t want %t", codec, requireGPU, gpuAvailable, pixelsAvailable, got, want)
					}
				}
			}
		}
	}
}

func TestDMABUFReopenChangesFormatWithoutRevivingOldCache(t *testing.T) {
	t.Setenv("ANYKVM_LINUX_GPU_JPEG", "1")
	for _, formats := range [][2]string{{"jpeg", "h264"}, {"h264", "jpeg"}, {"surface", "jpeg"}, {"surface", "h264"}, {"jpeg", "surface"}} {
		t.Run(formats[0]+"/"+formats[1], func(t *testing.T) {
			d := NewDMABUFEncoder(DMABUFEncoderOptions{RequireGPUEncoding: true, IdleRefresh: true})
			defer d.Close()
			freed := 0
			encode := func(format string) *Frame {
				t.Helper()
				d.Configure(0, 0, 30)
				d.SetGPUSurface(format == "surface")
				d.SetDecodeImage(false)
				source := dmabuf.Frame{FD: 7, Width: 4, Height: 4, Pitch: 16, CropWidth: 4, CropHeight: 4, FourCC: dmabuf.XRGB8888, DeviceMajor: 226, DeviceMinor: 128}
				var frame *Frame
				var err error
				switch format {
				case "surface":
					d.SetVideoCodec("rawvideo")
					frame, err = d.encodeFrame(&Frame{Width: 4, Height: 4, nativeBuffer: &dmaSurfaceFixture{freed: &freed}})
				case "h264":
					d.SetVideoCodec("h264")
					d.encoder = &dmaVideoFixture{}
					d.encodeOptions = videoenc.Options{Codec: "h264", Width: 4, Height: 4, FPS: 30, Rate: framerate.New(30, 1), Bitrate: 12000, DRMDeviceMajor: 226, DRMDeviceMinor: 128}
					frame, err = d.EncodeSource(source, 4, 4, time.Now(), 0)
				default:
					d.SetVideoCodec("mjpeg")
					d.jpegEncoder = &dmaJPEGFixture{}
					frame, err = d.EncodeSource(source, 4, 4, time.Now(), 0)
				}
				if err != nil || frame == nil {
					t.Fatalf("%s frame: %v", format, err)
				}
				return frame
			}
			old := encode(formats[0])
			defer old.Release()
			oldData := bytes.Clone(old.Data)
			d.Close()
			if refresh, err := d.Repeat(); refresh != nil || err != nil {
				t.Fatal("closed source returned its old cache", err)
			}
			current := encode(formats[1])
			defer current.Release()
			refresh, err := d.Repeat()
			if err != nil || refresh == nil {
				t.Fatal("reopened source lost idle refresh", err)
			}
			defer refresh.Release()
			if refresh.Format != current.Format || !bytes.Equal(old.Data, oldData) || old.nativeBuffer != nil || current.nativeBuffer != nil {
				t.Fatal("reopen reused old format, storage or producer lease")
			}
			if formats[0] == "surface" && freed != 0 {
				t.Fatal("reopen freed a surface still held by the previous consumer")
			}
		})
	}
}

func TestDMABUFScreenAndSPICEPoliciesAreIndependent(t *testing.T) {
	t.Setenv("ANYKVM_LINUX_GPU_JPEG", "0")
	screen := NewDMABUFEncoder(DMABUFEncoderOptions{})
	spice := NewDMABUFEncoder(DMABUFEncoderOptions{RequireGPUEncoding: true, IdleRefresh: true})
	defer screen.Close()
	defer spice.Close()
	// Check both orders: using one source must not change the other's fallback.
	for _, d := range []*DMABUFEncoder{spice, screen, screen, spice} {
		native := &dmabufFrame{encoder: d}
		_, handled, err := native.EncodeTexture(&textureEncoderFixture{}, true)
		if d == spice && (!handled || err == nil) || d == screen && (handled || err != nil) {
			t.Fatal("video fallback policy crossed sources")
		}
		_, handled, err = native.EncodeJPEG(4, 4, 80)
		if d == spice && (!handled || err == nil) || d == screen && (handled || err != nil) {
			t.Fatal("JPEG fallback policy crossed sources")
		}
	}
	screenFreed, spiceFreed := 0, 0
	for _, source := range []struct {
		encoder *DMABUFEncoder
		freed   *int
	}{{screen, &screenFreed}, {spice, &spiceFreed}} {
		source.encoder.SetVideoCodec("rawvideo")
		source.encoder.SetGPUSurface(true)
		frame, err := source.encoder.encodeFrame(&Frame{Width: 4, Height: 4, nativeBuffer: &dmaSurfaceFixture{freed: source.freed}})
		if err != nil || frame == nil {
			t.Fatal("GPU snapshot:", err)
		}
		frame.Release()
	}
	if screenFreed != 1 || spiceFreed != 0 {
		t.Fatal("KMS retained an idle snapshot or SPICE lost its cache")
	}
	screen.Close()
	if frame, err := screen.Repeat(); frame != nil || err != nil {
		t.Fatal("KMS acquired SPICE's idle cache", err)
	}
	refresh, err := spice.Repeat()
	if err != nil || refresh == nil {
		t.Fatal("KMS close invalidated SPICE's idle surface", err)
	}
	spice.Close()
	if spiceFreed != 0 {
		t.Fatal("SPICE close freed a surface held by downstream")
	}
	refresh.Release()
	if spiceFreed != 1 {
		t.Fatal("cached SPICE surface was not released")
	}
}
