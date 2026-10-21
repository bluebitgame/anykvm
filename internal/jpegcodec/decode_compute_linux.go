//go:build linux && cgo

package jpegcodec

import (
	"anykvm/internal/gpuconv"
	_ "embed"
)

//go:embed shaders/jpeg_decode.spv
var jpegDecodeEntropy []byte

//go:embed shaders/jpeg_decode_inverseDCT.spv
var jpegDecodeIDCT []byte

//go:embed shaders/jpeg_decode_packNV12.spv
var jpegDecodePack []byte

type vulkanJPEGDecoder struct{ decoder *gpuconv.JPEGDecoder }

func newComputeJPEGBackend() (computeJPEGBackend, error) {
	d, err := gpuconv.NewJPEGDecoder([3][]byte{jpegDecodeEntropy, jpegDecodeIDCT, jpegDecodePack})
	if err != nil {
		return nil, err
	}
	return &vulkanJPEGDecoder{decoder: d}, nil
}
func (d *vulkanJPEGDecoder) decode(j *computeJPEG) ([]byte, error) {
	return d.decoder.Decode(j.entropy, j.metadata)
}
func (d *vulkanJPEGDecoder) Close() { d.decoder.Close() }
