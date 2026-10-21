package gamestream

import (
	"anykvm/internal/av1"
	"errors"
	"log"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
)

// streamEncodedVideo forwards capture access units without decoding or re-encoding.
func (x *streamSession) streamEncodedVideo(first bridge.Frame, v *videoPacketizer, epoch time.Time) (bridge.Frame, error) {
	x.logVideoEncoder(x.options.Codec, "passthrough")
	if first.Width != x.options.Width || first.Height != x.options.Height {
		log.Printf("gamestream: source is %dx%d, client requested %dx%d; preserving encoded source (match client resolution if decoding fails)", first.Width, first.Height, x.options.Width, x.options.Height)
	}
	current := first
	framePacer := sourceFramePacer{fps: x.options.FPS}
	countedCurrent := false
	waitingIDR := true
	wantIDR := false
	nativePending := false
	recoveryRestarts := 0
	lastRestart := time.Now()
	statsStart := time.Now()
	var lastSent time.Time
	var refreshRequested time.Time
	var maxGap, maxSend time.Duration
	frames, idrs := 0, 0
	received, recoverySeen, noPeer, awaitingIDR := 0, 0, 0, 0
	requestRefresh := func() error {
		if refreshRequested.IsZero() {
			refreshRequested = time.Now()
		}
		waitingIDR = true
		if request := x.server.cfg.OnVideoKeyframe; request != nil {
			handled, err := request()
			if err != nil {
				return err
			}
			if handled {
				log.Printf("gamestream: requested native encoder IDR")
				wantIDR, nativePending = false, true
				return nil
			}
		}
		if allowed := x.server.cfg.OnVideoRestartAllowed; allowed != nil && !allowed() {
			wantIDR, nativePending = false, false
			log.Printf("gamestream: waiting for source IDR without reopening sender-driven capture")
			return nil
		}
		wantIDR = true
		return nil
	}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if current.Data != nil {
			if !countedCurrent {
				received++
				if isRecoveryFrame(current.Data, current.Format) {
					recoverySeen++
				}
				countedCurrent = true
			}
			if current.Format != x.options.videoFormat() {
				return current, errors.New("capture format differs from negotiated video codec")
			}
			x.input.geometryMu.Lock()
			x.input.sourceW, x.input.sourceH = current.SourceWidth, current.SourceHeight
			if x.input.sourceW <= 0 || x.input.sourceH <= 0 {
				x.input.sourceW, x.input.sourceH = current.Width, current.Height
			}
			x.input.outputW, x.input.outputH = current.Width, current.Height
			x.input.nativeEncoded = true
			x.input.geometryMu.Unlock()
			x.mu.Lock()
			peer := x.videoPeer
			x.mu.Unlock()
			idr := isRecoveryFrame(current.Data, current.Format)
			if peer == nil {
				if allowed := x.server.cfg.OnVideoRestartAllowed; allowed != nil && !allowed() {
					// The phone can already be mirroring when PLAY opens capture.
					// Retain its first IDR until the client's UDP endpoint is known;
					// dropping it would require source recovery we cannot request.
					select {
					case <-x.ctx.Done():
						return current, nil
					case <-tick.C:
					case <-x.keyframe:
						if err := requestRefresh(); err != nil {
							return current, err
						}
					}
					continue
				}
				waitingIDR = true
				noPeer++
			} else if !waitingIDR || idr {
				if idr {
					// Startup requests may have queued while waiting for the phone.
					// This IDR satisfies them; do not ask for another immediately
					// after sending the very first native recovery frame.
					select {
					case <-x.keyframe:
					default:
					}
				}
				// TCP input accumulated before PLAY must not flood Moonlight's
				// bounded decoder queue. UDP burst pacing only spaces packets inside
				// a picture; source pacing spaces whole pictures without dropping
				// H.264 references or changing their RTP capture timestamps.
				if err := waitVideoPacing(x.ctx, framePacer.reserve(current.CapturedAt, time.Now())); err != nil {
					return current, err
				}
				sendStart := time.Now()
				batch, err := v.packBatch(current.Data, idr, captureRTPTimestamp(current.CapturedAt, epoch, sendStart), x.options.PacketSize, x.options.MinFEC)
				if err != nil {
					return current, err
				}
				err = v.pacer.send(x.ctx, x.server.videoSocket, peer, batch.packets)
				batch.Release()
				if err != nil {
					return current, err
				}
				current.Timing.Sent("gamestream", sendStart)
				waitingIDR = false
				if idr {
					if v.frame == 1 && refreshRequested.IsZero() {
						log.Printf("gamestream: sent first recovery frame=%d bytes=%d", v.frame, len(current.Data))
					}
					if !refreshRequested.IsZero() {
						log.Printf("gamestream: sent recovery frame=%d bytes=%d after=%.1fms", v.frame, len(current.Data), float64(time.Since(refreshRequested))/float64(time.Millisecond))
						refreshRequested = time.Time{}
					}
					// A naturally occurring IDR satisfies a pending refresh. Avoid
					// closing/reopening capture after recovery already happened.
					wantIDR = false
					nativePending, recoveryRestarts = false, 0
					idrs++
				}
				now := time.Now()
				maxSend = max(maxSend, now.Sub(sendStart))
				if !lastSent.IsZero() {
					maxGap = max(maxGap, now.Sub(lastSent))
				}
				lastSent = now
				frames++
			} else {
				awaitingIDR++
			}
			current.Release()
		}
		select {
		case <-x.ctx.Done():
			return current, nil
		case current = <-x.frames:
			countedCurrent = false
		case <-x.keyframe:
			if err := requestRefresh(); err != nil {
				return current, err
			}
		case <-tick.C:
			now := time.Now()
			x.mu.Lock()
			peerReady := x.videoPeer != nil
			x.mu.Unlock()
			if elapsed := now.Sub(statsStart); elapsed >= 5*time.Second {
				log.Printf("gamestream: video %.1f fps target=%d idrs=%d max_gap=%.1fms max_packetize_send=%.1fms received=%d recovery_seen=%d dropped_no_peer=%d dropped_waiting_IDR=%d peer_ready=%t waiting_IDR=%t", float64(frames)/elapsed.Seconds(), x.options.FPS, idrs, float64(maxGap)/float64(time.Millisecond), float64(maxSend)/float64(time.Millisecond), received, recoverySeen, noPeer, awaitingIDR, peerReady, waitingIDR)
				statsStart, frames, idrs, maxGap, maxSend = now, 0, 0, 0, 0
				received, recoverySeen, noPeer, awaitingIDR = 0, 0, 0, 0
			}
			if !waitingIDR || !peerReady {
				continue
			}
			if allowed := x.server.cfg.OnVideoRestartAllowed; allowed != nil && !allowed() {
				// A passive source cannot be restarted to force an IDR. Keep
				// the connection alive while waiting for a natural recovery frame,
				// including when an earlier native request receives no response.
				if refreshRequested.IsZero() {
					if err := requestRefresh(); err != nil {
						return current, err
					}
				}
				continue
			}
			// The initial recovery frame may arrive before the client's UDP
			// endpoint. Request a fresh one once it is possible to send it.
			if refreshRequested.IsZero() {
				if err := requestRefresh(); err != nil {
					return current, err
				}
			}
			// A successful request callback means feedback was sent, not that
			// the remote encoder honored it. Bound the wait before reopening.
			if nativePending && now.Sub(refreshRequested) >= 2*time.Second {
				log.Printf("gamestream: native IDR request timed out after %.1fms; capture recovery required", float64(now.Sub(refreshRequested))/float64(time.Millisecond))
				wantIDR, nativePending = true, false
			}
			if !wantIDR || time.Since(lastRestart) < time.Second {
				continue
			}
			if x.server.cfg.OnVideoRestart == nil {
				return current, errors.New("direct video capture cannot restart for IDR")
			}
			if recoveryRestarts >= 2 {
				return current, errors.New("direct capture produced no recovery frame after two restarts")
			}
			// The capture stop channel unblocks any producer waiting to enqueue a
			// P-frame while we synchronously close and reopen capture.
			log.Printf("gamestream: restarting capture for pending client IDR request")
			if err := x.server.cfg.OnVideoRestart(); err != nil {
				return current, err
			}
			waitingIDR, wantIDR = true, false
			lastRestart = time.Now()
			refreshRequested = lastRestart
			nativePending = true
			recoveryRestarts++
		}
	}
}

// Keep encoding and sender scheduling jitter out of the client's playback clock.
// RTP timestamps wrap at 32 bits, including captures before epoch.
func captureRTPTimestamp(capturedAt, epoch, fallback time.Time) uint32 {
	if capturedAt.IsZero() {
		capturedAt = fallback
	}
	elapsed := capturedAt.Sub(epoch)
	// Split seconds before multiplication and round the fractional tick. This
	// preserves NTSC intervals without overflow during long sessions.
	ticks := int64(elapsed/time.Second) * 90000
	fraction := int64(elapsed%time.Second) * 90000
	if fraction >= 0 {
		ticks += (fraction + int64(time.Second)/2) / int64(time.Second)
	} else {
		ticks += (fraction - int64(time.Second)/2) / int64(time.Second)
	}
	return uint32(ticks)
}

func isRecoveryFrame(data []byte, format capture.VideoFormat) bool {
	if format == capture.FormatAV1 {
		info, err := av1.Inspect(data)
		return err == nil && info.Recovery
	}
	if format != capture.FormatHEVC {
		return hasNAL(data, 5)
	}
	// GameStream accepts HEVC IRAP pictures (BLA, IDR and CRA). Require their
	// parameter sets as well so recovery never starts with an undecodable frame.
	var vps, sps, pps, irap bool
	for i := 0; i+4 < len(data); i++ {
		if data[i] != 0 || data[i+1] != 0 || data[i+2] != 1 {
			continue
		}
		t := (data[i+3] >> 1) & 63
		switch t {
		case 32:
			vps = true
		case 33:
			sps = true
		case 34:
			pps = true
		}
		if t >= 16 && t <= 21 {
			irap = true
		}
	}
	return vps && sps && pps && irap
}
