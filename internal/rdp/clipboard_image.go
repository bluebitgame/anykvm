package rdp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"strings"
)

const maxClipboardImagePixels = 64 << 20

// pngToDIBV5 encodes a top-down, alpha-preserving 32-bit DIBV5. CF_DIBV5
// carries a DIB directly, without the 14-byte BMP file header.
func pngToDIBV5(data []byte) ([]byte, error) {
	if err := validateClipboardPNG(data); err != nil {
		return nil, err
	}
	image, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	bounds := image.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w < 1 || h < 1 || uint64(w)*uint64(h) > maxClipboardImagePixels {
		return nil, errors.New("invalid clipboard image dimensions")
	}
	pixelBytes := uint64(w) * uint64(h) * 4
	if pixelBytes+124 > maxClipboardMessage {
		return nil, errors.New("clipboard image exceeds channel limit")
	}
	dib := make([]byte, 124+int(pixelBytes))
	binary.LittleEndian.PutUint32(dib, 124)
	binary.LittleEndian.PutUint32(dib[4:], uint32(w))
	binary.LittleEndian.PutUint32(dib[8:], uint32(-h)) // top-down
	binary.LittleEndian.PutUint16(dib[12:], 1)
	binary.LittleEndian.PutUint16(dib[14:], 32)
	binary.LittleEndian.PutUint32(dib[16:], 3) // BI_BITFIELDS
	binary.LittleEndian.PutUint32(dib[20:], uint32(pixelBytes))
	binary.LittleEndian.PutUint32(dib[40:], 0x00ff0000)
	binary.LittleEndian.PutUint32(dib[44:], 0x0000ff00)
	binary.LittleEndian.PutUint32(dib[48:], 0x000000ff)
	binary.LittleEndian.PutUint32(dib[52:], 0xff000000)
	binary.LittleEndian.PutUint32(dib[56:], 0x73524742) // LCS_sRGB
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			pixel := color.NRGBAModel.Convert(image.At(bounds.Min.X+x, bounds.Min.Y+y)).(color.NRGBA)
			offset := 124 + (y*w+x)*4
			dib[offset], dib[offset+1], dib[offset+2], dib[offset+3] = pixel.B, pixel.G, pixel.R, pixel.A
		}
	}
	return dib, nil
}

func validateClipboardPNG(data []byte) error {
	if len(data) == 0 || len(data) > maxClipboardMessage {
		return errProtocol
	}
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width < 1 || config.Height < 1 || uint64(config.Width)*uint64(config.Height) > maxClipboardImagePixels {
		return errors.New("invalid clipboard PNG")
	}
	return nil
}

// encodedClipboardImageToPNG accepts registered raster formats commonly
// published by Windows applications. The macOS backend stores PNG, so JPEG and
// GIF are decoded only after their dimensions pass the same memory bound.
func encodedClipboardImageToPNG(data []byte, encoding string) ([]byte, error) {
	if strings.EqualFold(encoding, clipPNGName) {
		if err := validateClipboardPNG(data); err != nil {
			return nil, err
		}
		return append([]byte(nil), data...), nil
	}
	if len(data) == 0 || len(data) > maxClipboardMessage {
		return nil, errProtocol
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width < 1 || config.Height < 1 || uint64(config.Width)*uint64(config.Height) > maxClipboardImagePixels {
		return nil, errors.New("invalid clipboard raster image")
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, decoded); err != nil {
		return nil, err
	}
	if pngData.Len() > maxClipboardMessage {
		return nil, io.ErrShortBuffer
	}
	return pngData.Bytes(), nil
}

// dibToPNG accepts the common uncompressed RGB and bitfield CF_DIB/CF_DIBV5
// forms produced by Windows clients. Indexed, compressed, and color-managed
// DIBs are deliberately rejected rather than silently corrupting a paste.
func dibToPNG(data []byte) ([]byte, error) {
	if len(data) < 40 || len(data) > maxClipboardMessage {
		return nil, errProtocol
	}
	header := int(binary.LittleEndian.Uint32(data))
	if header < 40 || header > len(data) {
		return nil, errProtocol
	}
	w := int(int32(binary.LittleEndian.Uint32(data[4:])))
	rawH := int(int32(binary.LittleEndian.Uint32(data[8:])))
	if w < 1 || rawH == 0 || rawH == -1<<31 {
		return nil, errProtocol
	}
	topDown := rawH < 0
	h := rawH
	if h < 0 {
		h = -h
	}
	if uint64(w)*uint64(h) > maxClipboardImagePixels {
		return nil, errors.New("invalid clipboard image dimensions")
	}
	if binary.LittleEndian.Uint16(data[12:]) != 1 {
		return nil, errProtocol
	}
	bpp, compression := binary.LittleEndian.Uint16(data[14:]), binary.LittleEndian.Uint32(data[16:])
	if bpp != 24 && bpp != 32 || compression != 0 && compression != 3 {
		return nil, errors.New("unsupported clipboard DIB encoding")
	}
	offset := header
	red, green, blue, alpha := uint32(0x00ff0000), uint32(0x0000ff00), uint32(0x000000ff), uint32(0)
	if compression == 3 {
		if header >= 56 {
			red, green, blue = binary.LittleEndian.Uint32(data[40:]), binary.LittleEndian.Uint32(data[44:]), binary.LittleEndian.Uint32(data[48:])
			if header >= 56 {
				alpha = binary.LittleEndian.Uint32(data[52:])
			}
		} else {
			if len(data) < 52 {
				return nil, errProtocol
			}
			red, green, blue, offset = binary.LittleEndian.Uint32(data[40:]), binary.LittleEndian.Uint32(data[44:]), binary.LittleEndian.Uint32(data[48:]), 52
		}
		if red != 0x00ff0000 || green != 0x0000ff00 || blue != 0x000000ff || alpha != 0 && alpha != 0xff000000 {
			return nil, errors.New("unsupported clipboard DIB color masks")
		}
	}
	stride := ((uint64(w)*uint64(bpp) + 31) / 32) * 4
	if stride > uint64(len(data)) || uint64(offset)+stride*uint64(h) > uint64(len(data)) {
		return nil, errProtocol
	}
	image := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		sourceY := y
		if !topDown {
			sourceY = h - 1 - y
		}
		row := data[offset+int(stride)*sourceY:]
		for x := 0; x < w; x++ {
			if bpp == 24 {
				image.SetNRGBA(x, y, color.NRGBA{R: row[x*3+2], G: row[x*3+1], B: row[x*3], A: 255})
			} else {
				a := uint8(255)
				if alpha != 0 {
					a = row[x*4+3]
				}
				image.SetNRGBA(x, y, color.NRGBA{R: row[x*4+2], G: row[x*4+1], B: row[x*4], A: a})
			}
		}
	}
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, image); err != nil {
		return nil, err
	}
	if pngData.Len() > maxClipboardMessage {
		return nil, io.ErrShortBuffer
	}
	return pngData.Bytes(), nil
}
