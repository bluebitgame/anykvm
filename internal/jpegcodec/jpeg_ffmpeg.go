//go:build ffmpeg && cgo && (darwin || linux || windows)

package jpegcodec

import (
	"anykvm/internal/avmedia"
	"anykvm/internal/framebuf"
	"fmt"
	"runtime"
	"sync"
)

var ffmpegJPEGAvailable = sync.OnceValue(func() bool { return avmedia.HasEncoder("mjpeg") })

type ffmpegJPEGCodec struct {
	encoder, decoder       *avmedia.Session
	width, height, quality int
}

func (c *ffmpegJPEGCodec) close() {
	if c.encoder != nil {
		c.encoder.Close()
	}
	if c.decoder != nil {
		c.decoder.Close()
	}
}

var ffmpegJPEGCodecs = sync.Pool{New: func() any {
	c := new(ffmpegJPEGCodec)
	runtime.SetFinalizer(c, (*ffmpegJPEGCodec).close)
	return c
}}

func Backend() string {
	if ffmpegJPEGAvailable() {
		return "FFmpeg JPEG"
	}
	return fallbackBackend()
}

func encodeBGRA(pixels []byte, width, height, quality int) ([]byte, error) {
	buffer, err := encodeBGRAOwned(pixels, width, height, quality)
	if err != nil {
		return nil, err
	}
	defer buffer.Release()
	return append([]byte(nil), buffer.Bytes()...), nil
}

func encodeBGRAOwned(pixels []byte, width, height, quality int) (*framebuf.Buffer, error) {
	if !ffmpegJPEGAvailable() || width > 8192 || height > 8192 {
		return encodeFallbackOwned(pixels, width, height, quality)
	}
	c := ffmpegJPEGCodecs.Get().(*ffmpegJPEGCodec)
	defer ffmpegJPEGCodecs.Put(c)
	if c.encoder == nil || c.width != width || c.height != height || c.quality != quality {
		if c.encoder != nil {
			c.encoder.Close()
		}
		var err error
		c.encoder, err = avmedia.NewJPEGEncoder(width, height, quality)
		if err != nil {
			return encodeFallbackOwned(pixels, width, height, quality)
		}
		c.width, c.height, c.quality = width, height, quality
	}
	buffer, err := c.encoder.EncodeOwned(pixels, width, height, "bgra", false)
	if err == nil && buffer == nil {
		err = fmt.Errorf("FFmpeg JPEG: encoder returned no image")
	}
	if err != nil {
		c.encoder.Close()
		c.encoder = nil
		return encodeFallbackOwned(pixels, width, height, quality)
	}
	return buffer, nil
}

func decodeBGRA(data []byte, width, height int) ([]byte, error) {
	if !ffmpegJPEGAvailable() {
		return decodeFallback(data, width, height)
	}
	c := ffmpegJPEGCodecs.Get().(*ffmpegJPEGCodec)
	defer ffmpegJPEGCodecs.Put(c)
	if c.decoder == nil {
		var err error
		c.decoder, err = avmedia.NewDecoder("mjpeg")
		if err != nil {
			return decodeFallback(data, width, height)
		}
	}
	frame, err := c.decoder.Decode(data)
	if err != nil || frame == nil {
		// Unsupported scans use the platform fallback without introducing TurboJPEG.
		c.decoder.Close()
		c.decoder = nil
		return decodeFallback(data, width, height)
	}
	if frame.Width != width || frame.Height != height || len(frame.Data) != width*height*4 {
		return nil, fmt.Errorf("FFmpeg JPEG: decoded dimensions changed")
	}
	return frame.Data, nil
}
