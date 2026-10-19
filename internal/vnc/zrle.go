package vnc

import (
	"bytes"
	"compress/zlib"

	"anykvm/internal/pixelops"
)

const (
	zrleTileSize = 64
	zrleSubRaw   = 0
	zrleSubSolid = 1
	zrleSubRLE   = 128
)

// zlibStream is a persistent zlib compression stream. The RFB spec requires a
// single zlib "stream" object per connection for the zlib and ZRLE encodings,
// so that rectangles are decoded strictly in order. Each call to compress runs
// the data through the same underlying deflate state and flushes, emitting only
// the bytes produced for that rectangle (RFC 1950 zlib format).
type zlibStream struct {
	buf bytes.Buffer
	w   *zlib.Writer
}

func newZlibStream() *zlibStream {
	z := &zlibStream{}
	z.w, _ = zlib.NewWriterLevel(&z.buf, zlib.DefaultCompression)
	return z
}

// compress feeds data through the persistent stream, performs a sync flush and
// returns a copy of the compressed bytes generated for this call. The first
// call also emits the 2-byte zlib header.
func (z *zlibStream) compress(data []byte) []byte {
	z.buf.Reset()
	z.w.Write(data)
	z.w.Flush()
	out := make([]byte, z.buf.Len())
	copy(out, z.buf.Bytes())
	return out
}

// buildZRLETiles encodes BGRA pixel data into the uncompressed ZRLE tile stream
// (64x64 tiles, left-to-right, top-to-bottom) using CPIXELs of the negotiated
// pixel format. The result must then be passed through the connection's
// persistent zlib stream before transmission.
func buildZRLETiles(pixels []byte, width, height int, pf pixelFormat) []byte {
	var tiles bytes.Buffer
	stride := width * 4
	cpixelBuf := make([]byte, 4)

	for ty := 0; ty < height; ty += zrleTileSize {
		th := zrleTileSize
		if ty+th > height {
			th = height - ty
		}
		for tx := 0; tx < width; tx += zrleTileSize {
			tw := zrleTileSize
			if tx+tw > width {
				tw = width - tx
			}
			encodeZRLETile(&tiles, pixels, stride, tx, ty, tw, th, pf, cpixelBuf)
		}
	}

	return tiles.Bytes()
}

func encodeZRLETile(w *bytes.Buffer, pixels []byte, stride, tx, ty, tw, th int, pf pixelFormat, cpixelBuf []byte) {
	firstOff := ty*stride + tx*4
	firstPixel := pixels[firstOff : firstOff+4]
	solid := true

	for y := 0; y < th && solid; y++ {
		rowOff := (ty+y)*stride + tx*4
		row := pixels[rowOff : rowOff+tw*4]
		solid = row[0] == firstPixel[0] && row[1] == firstPixel[1] && row[2] == firstPixel[2] &&
			pixelops.RunBGR(row) == tw
	}

	if solid {
		w.WriteByte(zrleSubSolid)
		writeCPIXEL(w, firstPixel, pf, cpixelBuf)
		return
	}

	cb := pf.cpixelBytes()
	rleBuf := encodeRLETile(pixels, stride, tx, ty, tw, th, pf, cpixelBuf)
	rawSize := tw * th * cb

	if len(rleBuf) < rawSize {
		w.WriteByte(zrleSubRLE)
		w.Write(rleBuf)
	} else {
		w.WriteByte(zrleSubRaw)
		if pf.isNativeBGRA() && cb == 3 {
			w.Grow(rawSize)
			out := w.AvailableBuffer()[:rawSize]
			for y := 0; y < th; y++ {
				rowOff := (ty+y)*stride + tx*4
				pixelops.PackBGR(out[y*tw*3:(y+1)*tw*3], pixels[rowOff:rowOff+tw*4])
			}
			w.Write(out)
			return
		}
		for y := 0; y < th; y++ {
			rowOff := (ty+y)*stride + tx*4
			for x := 0; x < tw; x++ {
				writeCPIXEL(w, pixels[rowOff+x*4:], pf, cpixelBuf)
			}
		}
	}
}

// encodeRLETile encodes a tile as ZRLE plain RLE (subencoding 128). Each run is
// a CPIXEL followed by the run length, where the length is (runLength-1)
// expressed as zero or more 255 bytes plus a final byte < 255.
func encodeRLETile(pixels []byte, stride, tx, ty, tw, th int, pf pixelFormat, cpixelBuf []byte) []byte {
	var buf bytes.Buffer
	totalPixels := tw * th

	for i := 0; i < totalPixels; {
		y := i / tw
		x := i % tw
		off := (ty+y)*stride + (tx+x)*4
		curPixel := pixels[off : off+4]

		run := 0
		for i+run < totalPixels {
			ny := (i + run) / tw
			nx := (i + run) % tw
			noff := (ty+ny)*stride + (tx+nx)*4
			if pixels[noff] != curPixel[0] ||
				pixels[noff+1] != curPixel[1] ||
				pixels[noff+2] != curPixel[2] {
				break
			}
			count := pixelops.RunBGR(pixels[noff : noff+(tw-nx)*4])
			run += count
			if count < tw-nx {
				break
			}
		}

		if pf.isNativeBGRA() && pf.cpixelBytes() == 3 {
			buf.Write(curPixel[:3])
		} else {
			writeCPIXEL(&buf, curPixel, pf, cpixelBuf)
		}
		rl := run - 1
		for rl >= 255 {
			buf.WriteByte(255)
			rl -= 255
		}
		buf.WriteByte(byte(rl))
		i += run
	}

	return buf.Bytes()
}

// writeCPIXEL writes a CPIXEL for the negotiated pixel format. Input is native
// BGRA (bgra[0]=B, [1]=G, [2]=R). cpixelBuf is scratch space of length >= 4.
func writeCPIXEL(w *bytes.Buffer, bgra []byte, pf pixelFormat, cpixelBuf []byte) {
	n := pf.putCPIXEL(cpixelBuf, bgra[2], bgra[1], bgra[0])
	w.Write(cpixelBuf[:n])
}
