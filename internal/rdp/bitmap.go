package rdp

import (
	"errors"
	"fmt"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"anykvm/internal/gpuconv"
	"anykvm/internal/jpegcodec"
	"anykvm/internal/logging"
	"anykvm/internal/pixelops"
)

// Bitmap codecs are independently implemented from MS-RDPEGFX 2.2.4.1 and
// MS-RDPEGDI 2.2.2.5.1/3.1.9. This stateless ClearCodec encoder supplies the
// residual/raw candidate for the connection's cache-aware encoder.
func encodeClearCodec(pixels []byte, w, h int, sequence byte) []byte {
	residual := make([]byte, 0, len(pixels))
	for i := 0; i < len(pixels); {
		run := pixelops.RunBGR(pixels[i:])
		next := i + run*4
		residual = append(residual, pixels[i:i+3]...)
		if run < 255 {
			residual = append(residual, byte(run))
		} else {
			residual = append(residual, 255)
			if run < 65535 {
				residual = append(residual, u16(run)...)
			} else {
				residual = append(residual, 255, 255)
				residual = append(residual, u32(uint32(run))...)
			}
		}
		i = next
	}
	if len(residual) <= 3*w*h+13 {
		return join([]byte{0, sequence}, u32(uint32(len(residual))), u32(0), u32(0), residual)
	}
	// A raw BGR subcodec rectangle bounds expansion on noisy desktop images.
	raw := pixelops.PackBGR(make([]byte, 3*w*h), pixels)
	subcodec := join(u16(0), u16(0), u16(w), u16(h), u32(uint32(len(raw))), []byte{0}, raw)
	return join([]byte{0, sequence}, u32(0), u32(0), u32(uint32(len(subcodec))), subcodec)
}

func encodePlanar(pixels []byte, w, h int) []byte {
	e := planarEncoder{}
	return e.encodeRect(pixels, w, h, bitmapRect{0, 0, w, h}, progressiveBGRA)
}

func appendPlanarRow(out, row []byte) []byte {
	for i := 0; i < len(row); {
		start, run := i, 0
		for i < len(row) && i-start < 15 {
			i++
			run = pixelops.RunByte(row[i-1:]) - 1
			if run >= 3 {
				break
			}
			run = 0
		}
		combined := run
		if combined > 15 {
			combined = 0
		}
		out = append(out, byte((i-start)<<4|combined))
		out = append(out, row[start:i]...)
		i += combined
		run -= combined
		for run != 0 {
			n := min(run, 47)
			if n >= 32 {
				out = append(out, byte((n-32)<<4|2))
			} else if n >= 16 {
				out = append(out, byte((n-16)<<4|1))
			} else if n >= 3 {
				out = append(out, byte(n))
			} else {
				out = append(out, byte(n<<4))
				out = append(out, row[i:i+n]...)
			}
			i += n
			run -= n
		}
	}
	return out
}

type bitmapRect struct{ x, y, w, h int }

func dirtyBitmapRects(pixels, previous []byte, w, h int) []bitmapRect {
	blocks := pixelops.DirtyBlocks(previous, pixels, w, h, 0)
	var rects []bitmapRect
	if len(blocks) != 0 {
		rects = make([]bitmapRect, len(blocks))
		for i, b := range blocks {
			rects[i] = bitmapRect{b.X, b.Y, b.W, b.H}
		}
	}
	return rects
}

// Keep detection on the shared 64px grid, but batch complete dirty 128px groups
// for ClearCodec/Planar. This bounds encoding overhead without adding unchanged
// neighbors to a sparse update. Progressive keeps its individual 64px tiles.
func coalesceBitmapRects(rects []bitmapRect, w, h int) []bitmapRect {
	if len(rects) < 2 {
		return rects
	}
	const group = 2 * pixelops.BlockSize
	counts := make(map[[2]int]int)
	for _, r := range rects {
		counts[[2]int{r.x / group * group, r.y / group * group}]++
	}
	out := make([]bitmapRect, 0, len(rects))
	for _, r := range rects {
		key := [2]int{r.x / group * group, r.y / group * group}
		count := counts[key]
		if count == 0 {
			continue
		}
		gw, gh := min(group, w-key[0]), min(group, h-key[1])
		if count == ((gw+63)/64)*((gh+63)/64) {
			out = append(out, bitmapRect{key[0], key[1], gw, gh})
			counts[key] = 0
		} else {
			out = append(out, r)
		}
	}
	return out
}

func dirtyProgressiveRects(pixels, previous []byte, w, h int) []bitmapRect {
	return dirtyProgressiveRectsFormat(pixels, previous, w, h, progressiveBGRA)
}

func dirtyBitmapRectsSized(pixels, previous []byte, w, h, size int) []bitmapRect {
	var rects []bitmapRect
	for y := 0; y < h; y += size {
		for x := 0; x < w; x += size {
			r := bitmapRect{x, y, min(size, w-x), min(size, h-y)}
			dirty := len(previous) != len(pixels)
			if !dirty {
				off := (y*w + x) * 4
				dirty = !pixelops.EqualBGRBlock(pixels[off:], previous[off:], w*4, r.w, r.h)
			}
			if dirty {
				rects = append(rects, r)
			}
		}
	}
	return rects
}

func bitmapTile(pixels []byte, stride int, r bitmapRect) []byte {
	tile := make([]byte, 0, r.w*r.h*4)
	for y := r.y; y < r.y+r.h; y++ {
		off := (y*stride + r.x) * 4
		tile = append(tile, pixels[off:off+r.w*4]...)
	}
	return tile
}

// Tighten an already-dirty tile to its changed pixels. Small repeated updates
// can then use the glyph cache without subdividing every frame into 32px tiles.
func trimBitmapRect(pixels, previous []byte, stride int, r bitmapRect) bitmapRect {
	if len(previous) != len(pixels) {
		return r
	}
	left, top, right, bottom := r.x+r.w, r.y+r.h, r.x, r.y
	for y := r.y; y < r.y+r.h; y++ {
		row := (y*stride + r.x) * 4
		if pixelops.EqualBGR(pixels[row:row+r.w*4], previous[row:row+r.w*4]) {
			continue
		}
		for x := r.x; x < r.x+r.w; x++ {
			i := (y*stride + x) * 4
			if pixels[i] != previous[i] || pixels[i+1] != previous[i+1] || pixels[i+2] != previous[i+2] {
				left, top, right, bottom = min(left, x), min(top, y), max(right, x+1), max(bottom, y+1)
			}
		}
	}
	return bitmapRect{left, top, max(0, right-left), max(0, bottom-top)}
}

func graphicsBitmap(data []byte, r bitmapRect, codec displayCodec) []byte {
	return gfxPDU(1, join(u16(1), u16(int(codec)), []byte{0x20},
		u16(r.x), u16(r.y), u16(r.x+r.w), u16(r.y+r.h), u32(uint32(len(data))), data))
}

func bitmapBGRA(data []byte, w, h int, format string) ([]byte, error) {
	expected := w * h * 4
	switch format {
	case "bgra":
	case "nv12", "yuv420p":
		expected = w*h + 2*((w+1)/2)*((h+1)/2)
	case "uyvy422", "yuyv422":
		if w%2 != 0 {
			return nil, errors.New("odd packed YUV width")
		}
		expected = w * h * 2
	default:
		return nil, fmt.Errorf("unsupported bitmap pixels %q", format)
	}
	if len(data) != expected {
		return nil, fmt.Errorf("invalid %s bitmap frame length", format)
	}
	if format == "bgra" {
		return data, nil
	}
	out := make([]byte, w*h*4)
	clamp := func(v int) byte { return byte(min(255, max(0, v))) }
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var yy, u, v int
			cw, ch := (w+1)/2, (h+1)/2
			switch format {
			case "nv12":
				i := w*h + (y/2)*2*cw + (x/2)*2
				yy, u, v = int(data[y*w+x]), int(data[i]), int(data[i+1])
			case "yuv420p":
				i := (y/2)*cw + x/2
				yy, u, v = int(data[y*w+x]), int(data[w*h+i]), int(data[w*h+cw*ch+i])
			case "uyvy422":
				i := (y*w + (x &^ 1)) * 2
				yy, u, v = int(data[i+1+(x%2)*2]), int(data[i]), int(data[i+2])
			case "yuyv422":
				i := (y*w + (x &^ 1)) * 2
				yy, u, v = int(data[i+(x%2)*2]), int(data[i+1]), int(data[i+3])
			}
			// Match the pixel encoder's limited-range BT.709 conversion.
			yy, u, v = 298*(yy-16), u-128, v-128
			i := (y*w + x) * 4
			out[i], out[i+1], out[i+2], out[i+3] = clamp((yy+541*u+128)>>8), clamp((yy-55*u-136*v+128)>>8), clamp((yy+459*v+128)>>8), 255
		}
	}
	return out, nil
}

func (c *client) bitmapLoop() error {
	return c.bitmapLoopWithEncoder(nil)
}

func (c *client) bitmapLoopWithEncoder(progressive *progressiveEncoder) error {
	v := videoPipeline{}
	defer v.close()
	var previous []byte
	var damageState jpegcodec.DesktopDamageState
	scratchActive := false
	previousFormat := progressivePixelFormat(255)
	previousSurface := false
	w, h := 0, 0
	clear := newClearEncoder()
	planar := planarEncoder{}
	if c.codec == codecPlanar {
		logging.Infof("rdp: %s: bitmap codec=%s encoder=%q CLL=1", c.conn.RemoteAddr(), c.codec, planar.backend())
	}
	if c.codec == codecClear {
		logging.Infof("rdp: %s: bitmap codec=%s encoder=Go pixelops=%q", c.conn.RemoteAddr(), c.codec, pixelops.Backend())
	}
	if c.codec == codecProgressive {
		var err error
		runtime := c.s.runtimeConfig()
		if progressive == nil {
			progressive, err = newProgressiveEncoder(runtime.ProgressiveQuantization)
		}
		if err != nil {
			return err
		}
		defer progressive.close()
		if err := progressive.setRefinement(runtime.ProgressiveRefinement); err != nil {
			return err
		}
		logging.Infof("rdp: %s: bitmap codec=%s encoder=%q quantization=%d refine=%d", c.conn.RemoteAddr(), c.codec, progressive.backend(), progressive.quantization, progressive.refineBits)
	}
	started := time.Now()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	statsStarted := started
	var frames, tiles int64
	var held bridge.Frame
	defer func() { held.Release() }()
	var refinementFrames, refinementTiles int64
	var encodeMax, sendMax, ackMax time.Duration
	for {
		held.Release()
		if progressive != nil && progressive.refineBits != 0 {
			// Fresh captures take priority. In the gaps, refine immediately and
			// resume on each ACK rather than waiting for a tile to stop changing.
			n, _, encoded, sent, err := c.sendProgressiveRefinement(progressive, started)
			if err != nil {
				return err
			}
			if n != 0 {
				refinementFrames++
				refinementTiles += int64(n)
				encodeMax, sendMax = max(encodeMax, encoded), max(sendMax, sent)
				continue
			}
		}
		select {
		case <-c.done:
			return nil
		case <-ticker.C:
			elapsed := time.Since(statsStarted).Seconds()
			logging.Debugf("rdp: %s: bitmap output=%.1f fps refine=%.1f fps tiles=%d refine_tiles=%d encode_max=%.1fms send_max=%.1fms ack_wait_max=%.1fms",
				c.conn.RemoteAddr(), float64(frames)/elapsed, float64(refinementFrames)/elapsed, tiles, refinementTiles,
				float64(encodeMax)/float64(time.Millisecond), float64(sendMax)/float64(time.Millisecond), float64(ackMax)/float64(time.Millisecond))
			statsStarted = time.Now()
			frames, tiles = 0, 0
			refinementFrames, refinementTiles = 0, 0
			encodeMax, sendMax, ackMax = 0, 0, 0
		case <-c.ack:
			// A released ACK slot can immediately carry the next refinement.
		case f := <-c.frames:
			held = f
			var frameAckWait, lateAckWait time.Duration
			if !predictionCoded(f.Format) {
				ackStarted := time.Now()
				next, _, err := c.waitCaptureWindow(f)
				f, held = next, next
				if err != nil {
					return err
				}
				ackEnded := time.Now()
				ackMax = max(ackMax, ackEnded.Sub(ackStarted))
				frameAckWait += f.Timing.Waited(ackStarted, ackEnded)
			}
			select {
			case <-c.done:
				return nil
			default:
			}
			processingStarted := time.Now()
			requestedW, requestedH := c.requestedOutputSize(f.Width, f.Height)
			refresh := c.reset.Swap(false)
			if refresh {
				previous = nil
				if progressive != nil {
					progressive.discardRefinements()
				}
				if v.decoder != nil {
					v.decoder.Close()
					v.decoder = nil
				}
				// Pixel frames are independently decodable. Refreshing them only
				// invalidates our bitmap cache; reopening capture would discard a
				// good first frame and repeat a potentially slow display switch.
				compressed := f.Format == capture.FormatH264 || f.Format == capture.FormatHEVC || f.Format == capture.FormatAV1
				if compressed && c.s.cfg.OnKeyframe != nil {
					c.s.cfg.OnKeyframe()
				}
			}
			surface := f.Surface != nil
			width, height := f.Width, f.Height
			var data []byte
			format := "bgra"
			var err error
			if surface {
				if progressive == nil || f.Format != capture.FormatSurface || f.Buffer == nil || f.Surface.Width != width || f.Surface.Height != height {
					return fmt.Errorf("invalid GPU capture for RDP Progressive")
				}
			} else {
				data, width, height, format, err = v.sourcePixels(f)
				if err != nil {
					return err
				}
				if len(data) == 0 {
					continue
				}
			}
			pixelFormat := progressiveBGRA
			var pixels []byte
			outputW, outputH := c.bitmapOutputSize(f, width, height)
			if surface {
				// Only coordinate metadata crosses the CPU. The surface shader
				// performs the fit/resample and keeps both comparison frames in VRAM.
				fit := fittedFrameRect(width, height, outputW, outputH, false)
				f.ContentRect = transformedContentRect(frameContentRect(f), width, height, fit)
				f.Width, f.Height = outputW, outputH
				width, height = outputW, outputH
			} else if outputW != width || outputH != height {
				pixels, err = bitmapBGRA(data, width, height, format)
				if err == nil {
					f.Width, f.Height = width, height
					pixels, f = scaleBitmap(pixels, f, outputW, outputH)
					width, height = outputW, outputH
				}
			} else if c.codec == codecProgressive || c.codec == codecPlanar {
				pixels, pixelFormat, err = progressiveInputPixels(data, width, height, format)
			} else {
				pixels, err = bitmapBGRA(data, width, height, format)
			}
			if err != nil {
				return err
			}
			layout := makeGraphicsLayout(width, height, requestedW, requestedH, c.scaledMapping)
			layoutChanged := c.graphics != layout
			resized := width != w || height != h
			if resized || layoutChanged {
				if w != 0 && c.codec == codecProgressive {
					if err := c.sendGFX(gfxPDU(3, join(u16(1), u32(1)))); err != nil {
						return err
					}
				}
				if progressive != nil {
					progressive.first = true
					progressive.discardRefinements()
				}
				previous = nil
			}
			modeChanged := previousSurface != surface
			if (c.codec == codecProgressive || c.codec == codecPlanar) && (previousFormat != pixelFormat || modeChanged) {
				name := "BGRA"
				if pixelFormat == progressiveNV12 {
					name = "NV12 (BT.709 video range)"
				}
				if surface {
					name = "GPU BGRA (immutable capture; GPU dirty detection)"
				}
				label := "Progressive"
				if c.codec == codecPlanar {
					label = "Planar"
				}
				logging.Infof("rdp: %s: %s input=%s", c.conn.RemoteAddr(), label, name)
				previous = nil
				previousFormat = pixelFormat
			}
			forceMoves := refresh || resized || layoutChanged || modeChanged || previousFormat != pixelFormat
			var moves []gpuconv.SurfaceMove
			damage := fittedProgressiveDamage(f.Damage, width, height)
			if progressive != nil {
				moves = progressive.moveCandidates(damage, damageState, width, height, forceMoves)
			}
			var copies []gpuconv.SurfaceMove
			comparison := previous
			if !surface && c.codec == codecProgressive {
				copies = validatedPixelMoves(moves, previous, pixels, width, height, pixelFormat)
				if len(copies) > 0 {
					comparison = copyProgressiveBaseline(previous, width, copies)
				}
			}
			var rects []bitmapRect
			prepareTime := time.Duration(0)
			if surface {
				started := time.Now()
				rects, copies, err = progressive.prepareSurfaceMoves(f.Surface, width, height, forceMoves, moves)
				prepareTime = time.Since(started)
				if err != nil {
					return err
				}
			} else if c.codec == codecProgressive || c.codec == codecPlanar {
				rects = dirtyProgressiveRectsFormat(pixels, comparison, width, height, pixelFormat)
				if c.codec == codecPlanar {
					rects = coalesceBitmapRects(rects, width, height)
				}
			} else {
				rects = dirtyBitmapRects(pixels, previous, width, height)
				rects = coalesceBitmapRects(rects, width, height)
			}
			if len(rects) == 0 && len(copies) == 0 {
				damageState.Commit(damage)
				continue
			}
			if predictionCoded(f.Format) {
				ackStarted := time.Now()
				if err := c.waitWindow(); err != nil {
					return err
				}
				ackEnded := time.Now()
				lateAckWait = ackEnded.Sub(ackStarted)
				ackMax = max(ackMax, lateAckWait)
				frameAckWait += f.Timing.Waited(ackStarted, ackEnded)
			}
			if resized || layoutChanged {
				if scratchActive {
					if err := c.sendGFX(gfxPDU(0xa, u16(progressiveMoveScratch))); err != nil {
						return err
					}
					scratchActive = false
				}
				if err := c.ensureGraphicsLayout(layout); err != nil {
					return err
				}
				w, h = width, height
				clear = newClearEncoder()
			}
			c.frameGeometry(f, w, h)
			c.ackMu.Lock()
			c.sent++
			id := c.sent
			c.ackMu.Unlock()
			batch := gfxPDU(0xb, join(u32(uint32(time.Since(started)/time.Millisecond)), u32(id)))
			encodeTime := prepareTime
			var sendTime time.Duration
			var firstWrite time.Time
			sendBatch := func(p []byte) error {
				writeStarted := time.Now()
				if firstWrite.IsZero() {
					firstWrite = writeStarted
				}
				err := c.sendGFX(p)
				sendTime += time.Since(writeStarted)
				return err
			}
			if len(copies) > 0 {
				// Preserve the old source coefficients until their final UPGRADE
				// has been generated, before any current-frame FIRST replaces them.
				refinements := progressive.moveSourceRefinements(copies, w, h)
				for i := 0; i < len(refinements); {
					end := progressiveBatchEnd(refinements, i)
					encodeStarted := time.Now()
					encoded, err := progressive.upgrade(refinements[i:end], id)
					encodeTime += time.Since(encodeStarted)
					if err != nil {
						return err
					}
					if len(batch)+graphicsProgressiveHeaderSize+len(encoded) > 256<<10 {
						if err := sendBatch(batch); err != nil {
							return err
						}
						batch = batch[:0]
					}
					batch = appendGraphicsProgressive(batch, encoded)
					i = end
				}
				batch = append(batch, graphicsProgressiveCopies(w, h, copies, !scratchActive)...)
				scratchActive = true
			}
			var progressivePackets [][]byte
			for index := 0; index < len(rects); {
				r := rects[index]
				index++
				encodeStarted := time.Now()
				if c.codec == codecClear {
					r = trimBitmapRect(pixels, previous, w, r)
					if r.w == 0 || r.h == 0 {
						continue
					}
				}
				var update, progressiveData []byte
				if c.codec == codecProgressive {
					end := progressiveBatchEnd(rects, index-1)
					var encoded []byte
					var err error
					if surface {
						encoded, err = progressive.encodeSurface(w, h, rects[index-1:end], id)
					} else {
						if len(progressivePackets) == 0 {
							windowEnd := progressiveWindowEnd(rects, index-1)
							progressivePackets, err = progressive.encodeWindow(pixels, w, h, rects[index-1:windowEnd], id, pixelFormat)
						}
						if err == nil {
							encoded, progressivePackets = progressivePackets[0], progressivePackets[1:]
						}
					}
					if err != nil {
						return err
					}
					index = end
					progressiveData = encoded
				} else if c.codec == codecPlanar {
					encoded := planar.encodeRect(pixels, w, h, r, pixelFormat)
					update = graphicsBitmap(encoded, r, c.codec)
				} else {
					tile := bitmapTile(pixels, w, r)
					encoded := clear.encode(tile, r.w, r.h)
					update = graphicsBitmap(encoded, r, c.codec)
				}
				encodeTime += time.Since(encodeStarted)
				updateSize := len(update)
				if progressiveData != nil {
					updateSize = graphicsProgressiveHeaderSize + len(progressiveData)
				}
				if len(batch)+updateSize > 256<<10 {
					if err := sendBatch(batch); err != nil {
						return err
					}
					batch = batch[:0]
				}
				if progressiveData != nil {
					packing := time.Now()
					batch = appendGraphicsProgressive(batch, progressiveData)
					encodeTime += time.Since(packing)
				} else {
					batch = append(batch, update...)
				}
			}
			batch = append(batch, gfxPDU(0xc, u32(id))...)
			if err := sendBatch(batch); err != nil {
				return err
			}
			processing := time.Since(processingStarted) - lateAckWait - sendTime
			f.Timing.SentRDP("rdp-bitmap", firstWrite, processing, sendTime, frameAckWait)
			if surface {
				committing := time.Now()
				if err := progressive.commitSurface(); err != nil {
					return err
				}
				encodeTime += time.Since(committing)
				previous = nil
			} else {
				previous = append(previous[:0], pixels...)
			}
			if progressive != nil && len(copies) > 0 {
				progressive.invalidateMovedTiles(copies, width)
			}
			damageState.Commit(damage)
			previousSurface = surface
			frames++
			tiles += int64(len(rects))
			encodeMax, sendMax = max(encodeMax, encodeTime), max(sendMax, sendTime)
		}
	}
}
