package gamestream

import (
	"anykvm/internal/avmedia"
	"anykvm/internal/framerate"
	"anykvm/internal/jpegcodec"
	"anykvm/internal/netstats"
	"anykvm/internal/videoenc"
	"bytes"
	"crypto/aes"
	"errors"
	"io"
	"log"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
)

func (x *streamSession) videoLoop() {
	x.videoLoopUsing(videoenc.New)
}

func (x *streamSession) videoLoopUsing(newEncoder func(videoenc.Options) (videoenc.Encoder, error)) {
	var first bridge.Frame
	defer func() { first.Release() }()
	// A direct sender-driven stream cannot necessarily fulfill a decoder's IDR
	// request after a placeholder transition. A local output encoder can, even
	// when restarting the capture source itself is forbidden.
	placeholderAllowed := x.server.cfg.OnVideoRestartAllowed == nil || x.server.cfg.OnVideoRestartAllowed()
	if policy := x.server.cfg.OnVideoPlaceholderAllowed; policy != nil && !placeholderAllowed {
		placeholderAllowed = policy()
	}
	var fallback <-chan time.Time
	if placeholderAllowed {
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		fallback = timer.C
	}
	waitingForSource := false
	select {
	case <-x.ctx.Done():
		return
	case first = <-x.frames:
	case <-fallback:
		waitingForSource = true
		log.Printf("gamestream: sending waiting video until capture supplies its first picture")
		first = bridge.Frame{Format: capture.FormatRaw, Width: x.options.Width, Height: x.options.Height, Data: make([]byte, x.options.Width*x.options.Height*4)}
	}
	v := &videoPacketizer{}
	v.pacer.mbps = x.server.cfg.VideoPacingMbps
	log.Printf("gamestream: UDP burst pacing %d Mbps", v.pacer.bytesPerSecond()*8/1000000)
	if x.options.Encryption&2 != 0 {
		v.gcm = x.gcm
	}
	epoch := time.Now()
	if !first.CapturedAt.IsZero() && (first.PreserveSource || first.Format == capture.FormatH264 || first.Format == capture.FormatHEVC || first.Format == capture.FormatAV1) {
		// Native input can predate PLAY by seconds. Starting its clock at PLAY
		// would wrap the first RTP timestamp to almost 2^32 and then jump back
		// to zero. Keep a small nonzero origin (zero asks Moonlight to synthesize
		// a PTS) and preserve subsequent capture intervals.
		epoch = first.CapturedAt.Add(-time.Millisecond)
	}
	for x.ctx.Err() == nil {
		var next bridge.Frame
		var err error
		if first.Format == capture.FormatH264 || first.Format == capture.FormatHEVC || first.Format == capture.FormatAV1 {
			next, err = x.streamEncodedVideo(first, v, epoch)
		} else {
			next, err = x.encodeVideoUsing(first, v, epoch, newEncoder)
		}
		first = next
		if waitingForSource && err == nil && x.ctx.Err() == nil && next.Width > 0 {
			// The source picture may already be old when it replaces waiting
			// video. Anchor it to the current output clock once, avoiding a
			// backward RTP jump while preserving all later source intervals.
			if next.PreserveSource && !next.CapturedAt.IsZero() {
				epoch = next.CapturedAt.Add(-time.Since(epoch))
			}
			waitingForSource = false
			log.Printf("gamestream: capture ready; replacing waiting video")
		}
		if err != nil && x.ctx.Err() == nil {
			log.Printf("gamestream: video encoder: %v", err)
			x.stop("video encoder failed")
			return
		}
	}
}

func (x *streamSession) encodeVideo(first bridge.Frame, v *videoPacketizer, epoch time.Time) (bridge.Frame, error) {
	return x.encodeVideoUsing(first, v, epoch, videoenc.New)
}
func (x *streamSession) encodeVideoUsing(first bridge.Frame, v *videoPacketizer, epoch time.Time, newEncoder func(videoenc.Options) (videoenc.Encoder, error)) (bridge.Frame, error) {
	o := videoenc.Options{Width: x.options.Width, Height: x.options.Height, FPS: x.options.FPS, Bitrate: x.options.Bitrate, Codec: x.options.Codec}
	if x.server != nil {
		o.DisableLowLatencyRateControl = x.server.cfg.DisableLowLatencyRateControl
		o.H264Profile = x.server.cfg.H264Profile
	}
	divisor := 1
	if first.PreserveSource {
		o.Width, o.Height = first.Width, first.Height
		sourceRate := first.CaptureRate
		if !sourceRate.Valid() {
			sourceRate = framerate.New(first.CaptureFPS, 1)
		}
		if sourceRate.Valid() {
			o.Rate, divisor = sourceRate.Decimate(x.options.FPS)
			o.FPS = o.Rate.Round()
		}
		// Unknown variable cadence keeps the negotiated rate-control hint;
		// delivery still follows input frames rather than duplicating them.
	}
	o.Rate = o.Rate.Or(o.FPS)
	encoder, err := newEncoder(o)
	if err != nil {
		return first, err
	}
	defer encoder.Close()
	var jpegDecoder jpegcodec.VideoDecoder
	defer jpegDecoder.Close()
	x.logVideoEncoder(o.Codec, encoder.Name())
	x.input.geometryMu.Lock()
	x.input.sourceW = first.Width
	x.input.sourceH = first.Height
	x.input.outputW = o.Width
	x.input.outputH = o.Height
	x.input.nativeEncoded = false
	x.input.geometryMu.Unlock()
	var tickC <-chan time.Time
	if !first.PreserveSource {
		tick := time.NewTicker(time.Second / time.Duration(o.FPS))
		defer tick.Stop()
		tickC = tick.C
	}
	initialPending := first.PreserveSource
	var sourceFrame uint64
	current := first
	framePacer := sourceFramePacer{fps: o.FPS}
	statsStart := time.Now()
	var frames, idrs int
	var maxEncode, maxPacing, maxSend, maxGap time.Duration
	var lastSent time.Time
	force := true
	waitingIDR := true
	for {
		if initialPending {
			initialPending = false
		} else {
			select {
			case <-x.ctx.Done():
				return current, nil
			case <-x.keyframe:
				force = true
				if !first.PreserveSource {
					continue
				}
				// A passive source may be quiet. Its last decoded picture is
				// sufficient to produce a fresh local IDR without a new input.
			case f := <-x.frames:
				if f.Width != first.Width || f.Height != first.Height || f.Format != first.Format || f.PreserveSource != first.PreserveSource || (first.PreserveSource && (f.CaptureRate != first.CaptureRate || f.CaptureFPS != first.CaptureFPS)) {
					current.Release()
					return f, nil
				}
				current.Release()
				current = f
				if !first.PreserveSource {
					continue
				}
				sourceFrame++
				if sourceFrame%uint64(divisor) != 0 {
					continue
				}
			case <-tickC:
			}
		}
		pixels := current.Data
		pixelFormat, pixelW, pixelH := "bgra", current.Width, current.Height
		if current.Format == capture.FormatJPEG {
			decoded, err := jpegDecoder.Decode(current.Data, current.Width, current.Height, o.Width, o.Height)
			if err != nil {
				return current, err
			}
			pixels, pixelFormat, pixelW, pixelH = decoded.Data, decoded.Format, decoded.Width, decoded.Height
		}
		encodeStart := time.Now()
		encoded, err := videoenc.EncodePixelsOwned(encoder, o, pixels, pixelW, pixelH, pixelFormat, force)
		maxEncode = max(maxEncode, time.Since(encodeStart))
		if err != nil {
			return current, err
		}
		force = false
		if encoded == nil {
			continue
		}
		au := encoded.Bytes()
		x.mu.Lock()
		peer := x.videoPeer
		x.mu.Unlock()
		if peer == nil {
			encoded.Release()
			waitingIDR = true
			force = true
			continue
		}
		idr := isRecoveryFrame(au, x.options.videoFormat())
		if waitingIDR && !idr {
			encoded.Release()
			force = true
			continue
		}
		waitingIDR = false
		capturedAt := time.Time{}
		if first.PreserveSource {
			capturedAt = current.CapturedAt
			// Decoded input is coalesced to the newest raw picture while the
			// encoder is busy. Anchoring sends to the first source PTS would
			// replay startup latency on every coalesced picture: waiting then
			// consumes another newer picture with the same scheduling delay.
			// Live arrivals already supply source cadence; only cap catch-up
			// here. Retain actual capture PTS independently in the RTP header.
			pacingStart := time.Now()
			if err := waitVideoPacing(x.ctx, framePacer.reserve(time.Time{}, pacingStart)); err != nil {
				encoded.Release()
				return current, err
			}
			maxPacing = max(maxPacing, time.Since(pacingStart))
		}
		sendStart := time.Now()
		timestamp := captureRTPTimestamp(capturedAt, epoch, sendStart)
		batch, err := v.packBatch(au, idr, timestamp, x.options.PacketSize, x.options.MinFEC)
		encoded.Release() // The final FEC shards now own the packet payload.
		if err != nil {
			return current, err
		}
		err = v.pacer.send(x.ctx, x.server.videoSocket, peer, batch.packets)
		batch.Release()
		if err != nil {
			return current, err
		}
		now := time.Now()
		if lastSent.IsZero() {
			log.Printf("gamestream: first encoded picture frame=%d output=%dx%d rtp_timestamp=%d", v.frame, o.Width, o.Height, timestamp)
		}
		maxSend = max(maxSend, now.Sub(sendStart))
		if !lastSent.IsZero() {
			maxGap = max(maxGap, now.Sub(lastSent))
		}
		lastSent = now
		frames++
		if idr {
			idrs++
		}
		if elapsed := now.Sub(statsStart); elapsed >= 5*time.Second {
			log.Printf("gamestream: video %.1f fps source=%dx%d output=%dx%d idrs=%d max_gap=%.1fms max_encode=%.1fms max_pacing=%.1fms max_packetize_send=%.1fms", float64(frames)/elapsed.Seconds(), current.Width, current.Height, o.Width, o.Height, idrs, float64(maxGap)/float64(time.Millisecond), float64(maxEncode)/float64(time.Millisecond), float64(maxPacing)/float64(time.Millisecond), float64(maxSend)/float64(time.Millisecond))
			statsStart, frames, idrs = now, 0, 0
			maxGap, maxEncode, maxPacing, maxSend = 0, 0, 0, 0
		}
	}
}

func readAccessUnits(r io.Reader, emit func([]byte) error) error {
	buf := make([]byte, 32*1024)
	pending := make([]byte, 0, 128*1024)
	start, scan := -1, 0
	for {
		n, err := r.Read(buf)
		pending = append(pending, buf[:n]...)
		for scan+4 < len(pending) {
			prefix := 0
			if pending[scan] == 0 && pending[scan+1] == 0 {
				if pending[scan+2] == 1 {
					prefix = 3
				} else if pending[scan+2] == 0 && pending[scan+3] == 1 {
					prefix = 4
				}
			}
			if prefix > 0 && scan+prefix < len(pending) && pending[scan+prefix]&31 == 9 {
				if start >= 0 {
					au := append([]byte(nil), pending[start:scan]...)
					if e := emit(au); e != nil {
						return e
					}
					pending = append([]byte(nil), pending[scan:]...)
					scan = 0
				}
				start = scan
				scan += prefix
			} else {
				scan++
			}
		}
		if len(pending) > 8*1024*1024 {
			return errors.New("H.264 access unit exceeds buffer limit")
		}
		if err != nil {
			if err == io.EOF && start >= 0 && (hasNAL(pending[start:], 1) || hasNAL(pending[start:], 5)) {
				return emit(pending[start:])
			}
			return err
		}
	}
}
func hasNAL(data []byte, kind byte) bool {
	for i := 0; i+3 < len(data); i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 && data[i+3]&31 == kind {
			return true
		}
	}
	return false
}

const opusBitrateKbps = 128

func (x *streamSession) audioEnabled() bool {
	return x.machine >= 0 && x.machine < len(x.server.cfg.MachineAudio) && x.server.cfg.MachineAudio[x.machine]
}

func (x *streamSession) audioLoop() {
	if err := x.nativeAudioLoop(func() (nativeOpusEncoder, error) {
		encoder, err := newNativeOpusEncoder(x.options.AudioDuration)
		if err == nil {
			return encoder, nil
		}
		if !avmedia.Enabled {
			return nil, err
		}
		log.Printf("gamestream: system Opus unavailable: %v; using in-process libopus", err)
		session, err := avmedia.NewAudioEncoder(x.options.AudioDuration, opusBitrateKbps)
		if err != nil {
			return nil, err
		}
		return &libraryOpusEncoder{session: session}, nil
	}); err != nil && x.ctx.Err() == nil {
		// Audio is optional: capture or codec failures must not stop video/input.
		log.Printf("gamestream: audio disabled: %v", err)
	}
}

type libraryOpusEncoder struct{ session *avmedia.Session }

func (e *libraryOpusEncoder) Close() { e.session.Close() }
func (e *libraryOpusEncoder) Encode(data []byte) ([]byte, error) {
	return e.session.Encode(data, 0, 0, "s16", false)
}

func (x *streamSession) nativeAudioLoop(newEncoder func() (nativeOpusEncoder, error)) error {
	if !x.audioEnabled() {
		return nil
	}
	encoder, err := newEncoder()
	if err != nil {
		return err
	}
	defer func() {
		if encoder != nil {
			encoder.Close()
		}
	}()
	packetizer := audioPacketizer{keyID: x.keyID, duration: x.options.AudioDuration}
	if x.options.Encryption&4 != 0 {
		packetizer.block, _ = aes.NewCipher(x.key)
	}
	tick := time.NewTicker(time.Duration(x.options.AudioDuration) * time.Millisecond)
	defer tick.Stop()
	packetBytes := 48 * x.options.AudioDuration * 4
	var buffered []byte
	for {
		select {
		case <-x.ctx.Done():
			return nil
		case f := <-x.audio:
			buffered = append(buffered, f.PCM...)
			if len(buffered) > 48000*4/5 {
				buffered = append([]byte(nil), buffered[len(buffered)-48000*4/5:]...)
			}
		case <-tick.C:
			// Keep the configured encoder alive during gaps in capture.
			// Preserve partial PCM rather than generating synthetic silence.
			if len(buffered) < packetBytes {
				continue
			}
			pcm := buffered[:packetBytes]
			buffered = buffered[packetBytes:]
			packet, err := encoder.Encode(pcm)
			if err != nil {
				return err
			}
			if len(packet) == 0 {
				continue
			}
			packets, err := packetizer.pack(packet)
			if err != nil {
				return err
			}
			x.mu.Lock()
			peer := x.audioPeer
			x.mu.Unlock()
			if peer == nil {
				continue
			}
			for _, p := range packets {
				n, err := x.server.audioSocket.WriteToUDP(p, peer)
				netstats.Add(n)
				if err != nil {
					return err
				}
			}
		}
	}
}

// Parse the Ogg lacing table, preserving packets spanning multiple pages.
// Ogg headers and granule timestamps are container metadata, not RTP payload.
func readOpusPackets(r io.Reader, emit func([]byte) error) error {
	var packet []byte
	for {
		header := make([]byte, 27)
		if _, err := io.ReadFull(r, header); err != nil {
			return err
		}
		if string(header[:4]) != "OggS" || header[4] != 0 {
			return errors.New("invalid Ogg page")
		}
		lacing := make([]byte, int(header[26]))
		if _, err := io.ReadFull(r, lacing); err != nil {
			return err
		}
		for _, n := range lacing {
			data := make([]byte, int(n))
			if _, err := io.ReadFull(r, data); err != nil {
				return err
			}
			packet = append(packet, data...)
			if len(packet) > 65536 {
				return errors.New("Opus packet too large")
			}
			if n < 255 {
				if len(packet) > 0 && !bytes.HasPrefix(packet, []byte("OpusHead")) && !bytes.HasPrefix(packet, []byte("OpusTags")) {
					if err := emit(packet); err != nil {
						return err
					}
				}
				packet = nil
			}
		}
	}
}
