package capture

import (
	"fmt"
	"os"

	"anykvm/internal/dmabuf"
	"anykvm/internal/framebuf"
	"anykvm/internal/gpuconv"
	"anykvm/internal/jpegcodec"
	"anykvm/internal/logging"
	"anykvm/internal/videoenc"
)

// Bound immutable snapshots held by slow GPU consumers, including idle caches.
const (
	maxDMABUFSurfaceLeases = 4
	maxDMABUFSurfaceBytes  = 64 << 20
)

type dmabufJPEGEncoder interface {
	DRMDevice() (uint32, uint32)
	EncodeSource(source dmabuf.Frame, width, height, quality int) ([]byte, error)
	EncodeRegions(source dmabuf.Frame, width, height, quality int) (*jpegcodec.RestartJPEG, error)
	Close()
}

// dmabufFrame adapts a producer-owned draw to the shared format router.
// The caller of EncodeSource retains the FD and draw/fence for this entire call.
// GPU output and idle-refresh storage each acquire their own lifetime owner.
type dmabufFrame struct {
	encoder       *DMABUFEncoder
	source        dmabuf.Frame
	width, height int
}

func (f *dmabufFrame) DRMDevice() (uint32, uint32) { return f.source.DeviceMajor, f.source.DeviceMinor }
func (f *dmabufFrame) SnapshotSurface() (*gpuconv.Surface, *framebuf.Buffer, bool, error) {
	d := f.encoder
	if d.gpuSurfaceLeases.Load() >= maxDMABUFSurfaceLeases {
		return nil, nil, true, nil
	}
	if d.converter == nil {
		var err error
		d.converter, err = gpuconv.New(f.source.DeviceMajor, f.source.DeviceMinor)
		if err != nil {
			return nil, nil, true, err
		}
	}
	w, h := f.width, f.height
	if w*h > maxDMABUFSurfaceBytes/4 {
		return nil, nil, true, fmt.Errorf("GPU capture surface exceeds 64 MiB")
	}
	// Convert performs a GPU copy while the producer's DMA-BUF read fence is
	// held. The resulting allocation is immutable even if the scanout is reused.
	image, err := d.converter.Convert(f.source, w, h, false)
	if err != nil {
		return nil, nil, true, err
	}
	d.gpuSurfaceLeases.Add(1)
	owner := framebuf.New(nil, func() {
		image.Close()
		d.gpuSurfaceLeases.Add(-1)
	})
	d.gpuFrames++
	return &gpuconv.Surface{Width: w, Height: h, Image: image}, owner, true, nil
}
func (f *dmabufFrame) Submit(videoenc.PixelBufferSubmitter, uint64, bool) (bool, error) {
	return false, fmt.Errorf("DMA-BUF requires synchronous GPU submission")
}

// Release is intentionally empty: EncodeSource borrows the frame. The capture
// source closes its FD or acknowledges its SPICE draw after EncodeSource returns.
func (*dmabufFrame) Release() {}
func (f *dmabufFrame) EncodeTexture(e videoenc.Encoder, key bool) ([]byte, bool, error) {
	if encoder, ok := e.(videoenc.DMABUFEncoder); ok {
		data, handled, err := encoder.EncodeDMABUF(f.source, key)
		if handled && err == nil {
			f.encoder.gpuFrames++
		}
		return data, handled, err
	}
	if f.encoder.requireGPUEncoding {
		return nil, true, fmt.Errorf("video encoder cannot import DMA-BUF")
	}
	return nil, false, nil
}

func (*dmabufFrame) JPEGEncoderName() string { return "Vulkan Compute JPEG" }

func (f *dmabufFrame) EncodeJPEG(width, height, quality int) (*framebuf.Buffer, bool, error) {
	buffer, _, handled, err := f.encodeJPEG(width, height, quality, false)
	return buffer, handled, err
}
func (f *dmabufFrame) EncodeJPEGRegions(width, height, quality int) (*jpegcodec.RestartJPEG, bool, error) {
	_, snapshot, handled, err := f.encodeJPEG(width, height, quality, true)
	return snapshot, handled, err
}

func (f *dmabufFrame) encodeJPEG(width, height, quality int, regions bool) (*framebuf.Buffer, *jpegcodec.RestartJPEG, bool, error) {
	d := f.encoder
	if d.jpegDisabled || os.Getenv("ANYKVM_LINUX_GPU_JPEG") == "0" {
		if d.requireGPUEncoding {
			return nil, nil, true, fmt.Errorf("GPU JPEG disabled")
		}
		return nil, nil, false, nil
	}
	var err error
	if d.jpegEncoder == nil {
		d.jpegEncoder, err = newDMABUFJPEG(f.source.DeviceMajor, f.source.DeviceMinor)
		if err == nil {
			major, minor := d.jpegEncoder.DRMDevice()
			logging.Infof("capture: Vulkan JPEG DRM source=%d:%d renderer=%d:%d", f.source.DeviceMajor, f.source.DeviceMinor, major, minor)
		}
	}
	var data []byte
	var snapshot *jpegcodec.RestartJPEG
	if err == nil {
		if regions {
			snapshot, err = d.jpegEncoder.EncodeRegions(f.source, width, height, quality)
		} else {
			data, err = d.jpegEncoder.EncodeSource(f.source, width, height, quality)
		}
	}
	if err != nil {
		if d.jpegEncoder != nil {
			d.jpegEncoder.Close()
			d.jpegEncoder = nil
		}
		if d.requireGPUEncoding {
			return nil, nil, true, err
		}
		d.jpegDisabled = true
		logging.Infof("capture: Vulkan JPEG unavailable; using CPU JPEG: %v", err)
		return nil, nil, false, nil
	}
	d.gpuFrames++
	if regions {
		return nil, snapshot, true, nil
	}
	return framebuf.New(data, nil), nil, true, nil
}

// Materialize is the explicit pixel path. GPU video and JPEG attempt their
// native methods first; strict sources reject a video encoder's pixel fallback.
func (f *dmabufFrame) Materialize(out *Frame) error {
	d := f.encoder
	if d.requireGPUEncoding && (d.videoCodec == "h264" || d.videoCodec == "hevc" || d.videoCodec == "av1" || d.streamOptions != nil) {
		return fmt.Errorf("video encoder requires CPU pixel input")
	}
	if d.converter == nil {
		converter, err := gpuconv.New(f.source.DeviceMajor, f.source.DeviceMinor)
		if err != nil {
			return err
		}
		d.converter = converter
		logging.Infof("capture: DMA-BUF Vulkan BGRA readback for pixel consumer")
	}
	w, h := d.fitOutputSize(out.Width, out.Height)
	converted, err := d.converter.Convert(f.source, w, h, false)
	if err != nil {
		return err
	}
	defer converted.Close()
	data, err := converted.Readback()
	if err != nil {
		return err
	}
	out.Width, out.Height = w, h
	out.Data = data
	out.Format = FormatRaw
	out.nativeBuffer = nil
	d.readbacks++
	return nil
}

func (f *dmabufFrame) MaterializeVideo(out *Frame) error {
	if f.encoder.requireGPUEncoding {
		return fmt.Errorf("video encoder requires CPU pixel input")
	}
	return f.Materialize(out)
}
