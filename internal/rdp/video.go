package rdp

import (
	"bytes"
	"errors"
	"fmt"
	"image/jpeg"
	"log"
	"time"

	"anykvm/internal/avmedia"
	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"anykvm/internal/jpegcodec"
	"anykvm/internal/videoenc"
)

type videoPipeline struct {
	decoder                   *avmedia.Session
	decodeCodec               string
	encoder                   videoenc.Encoder
	loggedEncoder             string // connection-local path; no per-frame metadata
	options                   videoenc.Options
	sps, pps                  []byte
	direct, waiting, forceIDR bool
	outputW, outputH          int
}

// captureFPS follows the active target's capture cadence. RDP has no separate
// frame-rate setting. H.264 conversion needs a nominal encoder rate when the
// source omits it; this fallback must not throttle source-driven bitmap frames.
func captureFPS(f bridge.Frame) int {
	if f.CaptureFPS >= 1 && f.CaptureFPS <= 240 {
		return f.CaptureFPS
	}
	return 60
}

func (v *videoPipeline) close() {
	if v.decoder != nil {
		v.decoder.Close()
	}
	if v.encoder != nil {
		v.encoder.Close()
	}
}

func (v *videoPipeline) logEncoder(name string) {
	if name != v.loggedEncoder {
		log.Printf("rdp: video codec=h264 encoder=%q", name)
		v.loggedEncoder = name
	}
}

func annexNALs(p []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i+3 <= len(p); {
		n := 0
		if p[i] == 0 && p[i+1] == 0 {
			if p[i+2] == 1 {
				n = 3
			} else if i+4 <= len(p) && p[i+2] == 0 && p[i+3] == 1 {
				n = 4
			}
		}
		if n == 0 {
			i++
			continue
		}
		if start >= 0 && i > start {
			out = append(out, p[start:i])
		}
		start = i + n
		i += n
	}
	if start >= 0 && start < len(p) {
		out = append(out, p[start:])
	}
	return out
}

type bitReader struct {
	p      []byte
	bit    int
	failed bool
}

func (r *bitReader) bits(n int) uint32 {
	if n > 32 || r.bit+n > len(r.p)*8 {
		r.failed = true
		return 0
	}
	v := uint32(0)
	for range n {
		v = v<<1 | uint32((r.p[r.bit/8]>>(7-r.bit%8))&1)
		r.bit++
	}
	return v
}
func (r *bitReader) ue() uint32 {
	n := 0
	for !r.failed && r.bits(1) == 0 {
		n++
		if n > 30 {
			r.failed = true
			return 0
		}
	}
	if r.failed {
		return 0
	}
	return (1 << n) - 1 + r.bits(n)
}

// AVC420 requires eight-bit 4:2:0 H.264. An SPS is inspected before bypassing
// conversion; other H.264 profiles still go through the decoder and encoder.
func avc420SPS(nal []byte) bool {
	if len(nal) < 5 || nal[0]&31 != 7 {
		return false
	}
	rbsp := make([]byte, 0, len(nal))
	zeros := 0
	for _, b := range nal[1:] {
		if zeros == 2 && b == 3 {
			zeros = 0
			continue
		}
		rbsp = append(rbsp, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	r := bitReader{p: rbsp}
	profile := r.bits(8)
	r.bits(8)
	level := r.bits(8)
	r.ue()
	if level > 52 {
		return false
	}
	switch profile {
	case 66, 77:
		return !r.failed
	case 100:
		chroma := r.ue()
		if chroma != 1 {
			return false
		}
		luma, chromaDepth := r.ue(), r.ue()
		return !r.failed && luma == 0 && chromaDepth == 0
	default:
		return false
	}
}

// Capture scaling is applied by reopening the source on the desktop worker.
// Old access units can still be queued while the requested output changes.
// Do not decode those units just to scale them: the native encoder will produce
// the requested canvas, rounded up to even H.264 chroma dimensions.
func (v *videoPipeline) waitingForCaptureSize(f bridge.Frame) bool {
	return f.Format == capture.FormatH264 && f.CaptureScalesOutput && v.outputW > 0 && v.outputH > 0 &&
		(f.Width != (v.outputW+1)&^1 || f.Height != (v.outputH+1)&^1)
}

func (v *videoPipeline) frame(f bridge.Frame, bitrate, fps int) ([]byte, int, int, error) {
	if f.OutputMaxW > 0 && f.OutputMaxW < 2 || f.OutputMaxH > 0 && f.OutputMaxH < 2 {
		return nil, 0, 0, errors.New("Target resolution limit is too small for AVC420")
	}
	if f.Width < 2 || f.Height < 2 || f.Width > 8192 || f.Height > 8192 {
		return nil, 0, 0, errors.New("invalid video dimensions")
	}
	if v.waitingForCaptureSize(f) {
		// Dropping any prediction reference requires recovery with an IDR, even
		// if the client quickly returns to the previous size and SPS is unchanged.
		v.waiting, v.forceIDR = true, true
		return nil, 0, 0, nil
	}
	if f.Format == capture.FormatH264 {
		idr, picture := false, false
		for _, nal := range annexNALs(f.Data) {
			switch nal[0] & 31 {
			case 7:
				if !bytes.Equal(nal, v.sps) {
					v.waiting = true
					v.forceIDR = true
					v.sps = append(v.sps[:0], nal...)
					v.pps = nil
					v.direct = avc420SPS(nal)
				}
			case 8:
				v.pps = append(v.pps[:0], nal...)
			case 5:
				idr, picture = true, true
			case 1:
				picture = true
			}
		}
		if len(v.sps) == 0 || len(v.pps) == 0 || !picture || (v.waiting && !idr) {
			return nil, 0, 0, nil
		}
		v.waiting = false
		if idr {
			f.Data = join([]byte{0, 0, 0, 1}, v.sps, []byte{0, 0, 0, 1}, v.pps, f.Data)
		}
		if v.direct && (v.outputW == 0 || (v.outputW+1)&^1 == f.Width && (v.outputH+1)&^1 == f.Height) {
			v.logEncoder("passthrough")
			return f.Data, f.Width, f.Height, nil
		}
	}
	data, w, h, pixfmt, err := v.sourcePixels(f)
	if err != nil || len(data) == 0 {
		return nil, 0, 0, err
	}
	outputW, outputH := (w+1)&^1, (h+1)&^1
	if v.outputW > 0 && v.outputH > 0 {
		outputW, outputH = (v.outputW+1)&^1, (v.outputH+1)&^1
	}
	if v.encoder == nil || v.options.Width != outputW || v.options.Height != outputH || v.options.FPS != fps {
		if v.encoder != nil {
			v.encoder.Close()
			v.encoder = nil
		}
		v.options = videoenc.Options{Width: outputW, Height: outputH, FPS: fps, Bitrate: bitrate, OmitBitrate: bitrate == 0, Codec: "h264"}
		v.encoder, err = videoenc.New(v.options)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("H.264 encoder: %w", err)
		}
		v.forceIDR = true
	}
	encoded, err := videoenc.EncodePixels(v.encoder, v.options, data, w, h, pixfmt, v.forceIDR)
	if err == nil && len(encoded) != 0 {
		v.forceIDR = false
		v.logEncoder(v.encoder.Name())
	}
	return encoded, outputW, outputH, err
}

func (v *videoPipeline) sourcePixels(f bridge.Frame) ([]byte, int, int, string, error) {
	data, w, h, pixfmt := f.Data, f.Width, f.Height, "bgra"
	if w < 1 || h < 1 || w > 8192 || h > 8192 || w*h > (64<<20)/4 {
		return nil, 0, 0, "", errors.New("invalid source dimensions")
	}
	switch f.Format {
	case capture.FormatRaw:
	case capture.FormatYUV:
		pixfmt = f.PixFmt
	case capture.FormatJPEG:
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return nil, 0, 0, "", err
		}
		if cfg.Width != w || cfg.Height != h || w*h > (64<<20)/4 {
			return nil, 0, 0, "", errors.New("invalid JPEG dimensions")
		}
		decoded, _, _, err := jpegcodec.DecodeBGRA(data)
		if err != nil {
			return nil, 0, 0, "", err
		}
		data = decoded
	case capture.FormatH264, capture.FormatHEVC, capture.FormatAV1:
		codec := map[capture.VideoFormat]string{capture.FormatH264: "h264", capture.FormatHEVC: "hevc", capture.FormatAV1: "av1"}[f.Format]
		if v.decoder == nil || codec != v.decodeCodec {
			if v.decoder != nil {
				v.decoder.Close()
			}
			var err error
			v.decoder, err = avmedia.NewDecoder(codec)
			if err != nil {
				return nil, 0, 0, "", fmt.Errorf("%s source conversion: %w", codec, err)
			}
			v.decodeCodec = codec
		}
		decoded, err := v.decoder.Decode(data)
		if err != nil {
			return nil, 0, 0, "", err
		}
		if decoded == nil {
			return nil, 0, 0, "", nil
		}
		data, w, h = decoded.Data, decoded.Width, decoded.Height
	default:
		return nil, 0, 0, "", errors.New("unsupported capture format")
	}
	if w < 1 || h < 1 || w > 8192 || h > 8192 || w*h > (64<<20)/4 {
		return nil, 0, 0, "", errors.New("invalid decoded dimensions")
	}
	return data, w, h, pixfmt, nil
}

func (c *client) videoLoop() error {
	if c.codec != codecAVC420 {
		return c.bitmapLoop()
	}
	v := videoPipeline{waiting: true, forceIDR: true}
	defer v.close()
	w, h := 0, 0
	var layoutRequestW, layoutRequestH int
	started := time.Now()
	statsStarted := started
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var received, frames, skipped int64
	var processTotal, sendTotal, ackTotal, processMax, sendMax, ackMax time.Duration
	var held bridge.Frame
	defer func() { held.Release() }()
	for {
		held.Release()
		select {
		case <-c.done:
			return nil
		case <-ticker.C:
			elapsed := time.Since(statsStarted).Seconds()
			c.ackMu.Lock()
			pending, suspended := c.sent-c.acknowledged, c.suspendAck
			c.ackMu.Unlock()
			log.Printf("rdp: %s: video input=%.1f fps output=%.1f fps skipped=%d process_ms=%.1f/%.1f send_ms=%.1f/%.1f ack_wait_ms=%.1f/%.1f pending=%d ack_suspended=%t queued=%d (mean/max)",
				c.conn.RemoteAddr(), float64(received)/elapsed, float64(frames)/elapsed, skipped,
				float64(processTotal)/float64(max(received, 1))/float64(time.Millisecond), float64(processMax)/float64(time.Millisecond),
				float64(sendTotal)/float64(max(frames, 1))/float64(time.Millisecond), float64(sendMax)/float64(time.Millisecond),
				float64(ackTotal)/float64(max(frames, 1))/float64(time.Millisecond), float64(ackMax)/float64(time.Millisecond), pending, suspended, len(c.frames))
			statsStarted = time.Now()
			received, frames, skipped = 0, 0, 0
			processTotal, sendTotal, ackTotal, processMax, sendMax, ackMax = 0, 0, 0, 0, 0, 0
		case f := <-c.frames:
			received++
			held = f
			var frameAckWait time.Duration
			if !predictionCoded(f.Format) {
				ackStarted := time.Now()
				next, dropped, err := c.waitCaptureWindow(f)
				f, held = next, next
				if err != nil {
					return err
				}
				ackEnded := time.Now()
				waited := ackEnded.Sub(ackStarted)
				frameAckWait += f.Timing.Waited(ackStarted, ackEnded)
				ackTotal, ackMax = ackTotal+waited, max(ackMax, waited)
				received, skipped = received+int64(dropped), skipped+int64(dropped)
			}
			select {
			case <-c.done:
				return nil
			default:
			}
			requestedW, requestedH := c.requestedOutputSize(f.Width, f.Height)
			v.outputW, v.outputH = c.videoOutputSize(f)
			refresh := c.reset.Swap(false)
			if c.scaledMapping && (requestedW != layoutRequestW || requestedH != layoutRequestH) {
				refresh = true
			}
			layoutRequestW, layoutRequestH = requestedW, requestedH
			if refresh {
				v.waiting, v.forceIDR = true, true
				compressed := f.Format == capture.FormatH264 || f.Format == capture.FormatHEVC || f.Format == capture.FormatAV1
				if compressed && c.s.cfg.OnKeyframe != nil && !v.waitingForCaptureSize(f) {
					c.s.cfg.OnKeyframe()
				}
			}
			processStarted := time.Now()
			data, width, height, err := v.frame(f, c.s.runtimeConfig().Bitrate, captureFPS(f))
			processed := time.Since(processStarted)
			processTotal, processMax = processTotal+processed, max(processMax, processed)
			if err != nil {
				return err
			}
			if len(data) == 0 {
				skipped++
				continue
			}
			if predictionCoded(f.Format) {
				ackStarted := time.Now()
				if err := c.waitWindow(); err != nil {
					return err
				}
				ackEnded := time.Now()
				waited := ackEnded.Sub(ackStarted)
				frameAckWait += f.Timing.Waited(ackStarted, ackEnded)
				ackTotal, ackMax = ackTotal+waited, max(ackMax, waited)
			}
			select {
			case <-c.done:
				return nil
			default:
			}
			layout := makeGraphicsLayout(width, height, requestedW, requestedH, c.scaledMapping)
			if width != w || height != h || c.graphics != layout {
				if err := c.ensureGraphicsLayout(layout); err != nil {
					return err
				}
				w, h = width, height
			}
			f.ContentRect = frameContentRect(f)
			c.frameGeometry(f, w, h)
			c.ackMu.Lock()
			c.sent++
			id := c.sent
			c.ackMu.Unlock()
			packet := graphicsFrame(data, w, h, id, uint32(time.Since(started)/time.Millisecond))
			sendStarted := time.Now()
			if err := c.sendGFX(packet); err != nil {
				return err
			}
			sent := time.Since(sendStarted)
			f.Timing.SentRDP("rdp-gfx", sendStarted, processed, sent, frameAckWait)
			sendTotal, sendMax = sendTotal+sent, max(sendMax, sent)
			frames++
		}
	}
}
