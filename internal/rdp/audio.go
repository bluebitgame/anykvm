package rdp

import (
	"bytes"
	"fmt"
	"log"
	"sync"
	"time"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
)

// MS-RDPEA audio output travels on the client's rdpsnd static virtual channel.
// PCM is mandatory in the protocol and avoids a platform encoder dependency.
const (
	soundWave       = 2
	soundConfirm    = 5
	soundTraining   = 6
	soundFormats    = 7
	soundQuality    = 12
	soundWave2      = 13
	soundVersion    = 8
	audioChunkBytes = 48000 * 2 * 2 / 50 // at most 20 ms per packet
	audioQueueSize  = 5                  // at most 100 ms of queued audio
	audioWindow     = 16
	maxAudioMessage = 65539
	audioMaxAge     = 100 * time.Millisecond
	audioPlayWindow = 100 * time.Millisecond
)

var audioClock = time.Now()

func audioTicks(now time.Time) uint32 { return uint32(now.Sub(audioClock) / time.Millisecond) }

type audioChunk struct {
	pcm        []byte
	captured   uint32
	generation uint64
}

type audioPending struct {
	sent      time.Time
	stamp     uint16
	duration  time.Duration
	receipt   uint16
	confirmed bool
}

type audioState struct {
	mu sync.Mutex
	// Fragment assembly belongs exclusively to the protocol reader.
	buf                                       []byte
	total                                     int
	started, loopStarted, negotiated, trained bool
	trainingSent                              bool
	qualityPending                            bool
	formatsAt                                 time.Time
	version, format, trainingStamp            uint16
	nextBlock                                 byte
	queue                                     chan audioChunk
	wake                                      chan struct{}
	outstanding                               map[byte]audioPending
	received                                  map[byte]audioPending
	playbackConfirm                           bool
	generation                                uint64
	timeoutLogged                             bool
	statsStarted, statsLogged                 time.Time
	capturedDuration, sentDuration            time.Duration
	confirmCount, playbackCount               uint64
	maxConfirmHold                            time.Duration
}

func audioPDU(kind byte, body []byte) []byte {
	return join([]byte{kind, 0}, u16(len(body)), body)
}

func pcmAudioFormat() []byte {
	return join(u16(1), u16(2), u32(48000), u32(192000), u16(4), u16(16), u16(0))
}

func (c *client) startAudio() error {
	if c.data.audio == 0 {
		return nil
	}
	a := &c.audio
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.started {
		return nil
	}
	a.started = true
	a.nextBlock = 1 // The offered cLastBlockConfirmed is zero.
	a.queue, a.wake = make(chan audioChunk, audioQueueSize), make(chan struct{}, 1)
	a.outstanding = make(map[byte]audioPending)
	a.received = make(map[byte]audioPending)
	// No UDP port: audio shares the authenticated TLS connection with RDP.
	body := join(u32(0), u32(0), u32(0), u16(0), u16(1), []byte{0}, u16(soundVersion), []byte{0}, pcmAudioFormat())
	return c.sendStaticVC(c.data.audio, audioPDU(soundFormats, body))
}

func (c *client) handleAudioVC(p []byte) error {
	a := &c.audio
	if len(p) < 8 {
		return fmt.Errorf("%w: audio channel header", errProtocol)
	}
	total, flags := int(le.Uint32(p)), le.Uint32(p[4:])
	p = p[8:]
	if total < 4 || total > maxAudioMessage || flags & ^uint32(0x13) != 0 {
		return fmt.Errorf("%w: audio channel length/flags", errProtocol)
	}
	if flags&1 != 0 {
		if a.total != 0 {
			return fmt.Errorf("%w: overlapping audio fragments", errProtocol)
		}
		a.total, a.buf = total, a.buf[:0]
	}
	if a.total != total || len(p) > total-len(a.buf) {
		return fmt.Errorf("%w: audio fragment length", errProtocol)
	}
	a.buf = append(a.buf, p...)
	if flags&2 == 0 {
		return nil
	}
	if len(a.buf) != total {
		return fmt.Errorf("%w: incomplete audio message", errProtocol)
	}
	a.total = 0
	return c.handleAudio(a.buf)
}

func (c *client) handleAudio(p []byte) error {
	if len(p) < 4 || len(p) > maxAudioMessage || int(le.Uint16(p[2:])) != len(p)-4 {
		return fmt.Errorf("%w: audio PDU length", errProtocol)
	}
	a := &c.audio
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.started {
		return nil
	} // Ignore out-of-sequence optional channel messages.
	body := p[4:]
	switch p[0] {
	case soundFormats:
		if a.negotiated {
			return nil
		}
		if len(body) < 20 {
			return fmt.Errorf("%w: audio formats header", errProtocol)
		}
		count, version := int(le.Uint16(body[14:])), le.Uint16(body[17:])
		if count > 64 {
			return fmt.Errorf("%w: too many audio formats", errProtocol)
		}
		formats := body[20:]
		selected := -1
		for i := 0; i < count; i++ {
			if len(formats) < 18 {
				return fmt.Errorf("%w: truncated audio format", errProtocol)
			}
			n := 18 + int(le.Uint16(formats[16:]))
			if n > len(formats) {
				return fmt.Errorf("%w: audio format extra data", errProtocol)
			}
			if bytes.Equal(formats[:n], pcmAudioFormat()) && selected < 0 {
				selected = i
			}
			formats = formats[n:]
		}
		if len(formats) != 0 {
			return fmt.Errorf("%w: trailing audio formats", errProtocol)
		}
		a.negotiated, a.version = true, min(version, uint16(soundVersion))
		if selected < 0 || le.Uint32(body)&1 == 0 || version < 2 {
			log.Printf("rdp: audio disabled by client or no compatible PCM format")
			return nil
		}
		a.format = uint16(selected)
		if a.version >= 6 {
			a.qualityPending, a.formatsAt = true, time.Now()
			return nil
		}
		return c.sendAudioTrainingLocked()
	case soundTraining:
		if len(body) != 4 {
			return fmt.Errorf("%w: audio training confirmation", errProtocol)
		}
		if !a.trainingSent || a.trained || le.Uint16(body) != a.trainingStamp || le.Uint16(body[2:]) != 0 {
			return nil
		}
		a.trained = true
		a.statsStarted, a.statsLogged = time.Now(), time.Now()
		log.Printf("rdp: audio ready (48000 Hz stereo PCM, version=%d, TLS virtual channel)", a.version)
		a.signalLocked()
	case soundQuality:
		if len(body) != 4 {
			return fmt.Errorf("%w: audio quality mode", errProtocol)
		}
		// PCM has one fixed quality; all valid requested modes use this format.
		if a.qualityPending {
			a.qualityPending = false
			return c.sendAudioTrainingLocked()
		}
	case soundConfirm:
		if len(body) != 4 {
			return fmt.Errorf("%w: audio wave confirmation", errProtocol)
		}
		a.confirmLocked(body[2], le.Uint16(body), time.Now())
		a.signalLocked()
	}
	return nil
}

// Caller holds audio.mu. Modern clients send Quality Mode after their formats;
// when it is absent for one second, the worker uses the default dynamic mode.
func (c *client) sendAudioTrainingLocked() error {
	a := &c.audio
	a.trainingStamp = uint16(audioTicks(time.Now()))
	a.trainingSent = true
	// With no training payload, wPackSize must be zero (MS-RDPEA 2.2.3.1).
	return c.sendStaticVC(c.data.audio, audioPDU(soundTraining, join(u16(int(a.trainingStamp)), u16(0))))
}

// A receipt ACK does not mean that the device has played the samples. Detect
// clients that send an adjusted playback ACK, then retain their receipt-ACKed
// blocks until playback completes. Single-ACK clients keep the legacy window.
func (a *audioState) confirmLocked(block byte, stamp uint16, now time.Time) {
	pending, active := a.outstanding[block]
	if !active {
		var ok bool
		pending, ok = a.received[block]
		if !ok {
			return
		}
	}
	elapsed := int16(stamp - pending.stamp)
	if elapsed < 0 || (!active && now.Sub(pending.sent) >= 2*time.Second) {
		return
	}
	if pending.confirmed {
		// An identical retransmission must not be mistaken for playback.
		if int16(stamp-pending.receipt) <= 0 {
			return
		}
		a.playbackCount++
		a.maxConfirmHold = max(a.maxConfirmHold, time.Duration(elapsed)*time.Millisecond)
		if !a.playbackConfirm {
			a.playbackConfirm = true
			for id, p := range a.received {
				if now.Sub(p.sent) < 2*time.Second {
					a.outstanding[id] = p
				}
			}
			clear(a.received)
			log.Printf("rdp: audio playback confirmations detected; limiting unplayed PCM to 100ms")
		}
		delete(a.outstanding, block)
		return
	}
	a.confirmCount++
	a.maxConfirmHold = max(a.maxConfirmHold, time.Duration(elapsed)*time.Millisecond)
	pending.confirmed, pending.receipt = true, stamp
	if a.playbackConfirm {
		a.outstanding[block] = pending
	} else {
		delete(a.outstanding, block)
		a.received[block] = pending
	}
}

func (a *audioState) playbackQueuedLocked() time.Duration {
	if !a.playbackConfirm {
		return 0
	}
	var duration time.Duration
	for _, pending := range a.outstanding {
		duration += pending.duration
	}
	return duration
}

func (a *audioState) signalLocked() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// PushAudio admits bounded, owned PCM chunks without blocking capture/video.
// The shared bridge converts compressed sources before calling this sink.
func (s *Server) PushAudio(f capture.AudioFrame) {
	if len(f.Encoded) != 0 || len(f.PCM) == 0 || len(f.PCM)%4 != 0 || len(f.PCM) > 1<<20 ||
		(f.SampleRate != 0 && f.SampleRate != 48000) || (f.Channels != 0 && f.Channels != 2) {
		return
	}
	s.mu.Lock()
	c := s.active
	s.mu.Unlock()
	if c == nil {
		return
	}
	a := &c.audio
	a.mu.Lock()
	defer a.mu.Unlock()
	select {
	case <-c.done:
		return
	default:
	}
	if !a.trained {
		return
	}
	a.capturedDuration += time.Duration(len(f.PCM)) * time.Second / 192000
	// The first sample predates admission by the PCM duration; do not assign
	// future capture times when splitting a large source buffer.
	captured := audioTicks(time.Now()) - uint32(len(f.PCM)/192)
	// Only retain the newest 100 ms if a source provides an unusually big chunk.
	start := max(0, len(f.PCM)-audioQueueSize*audioChunkBytes)
	for off := start; off < len(f.PCM); off += audioChunkBytes {
		end := min(off+audioChunkBytes, len(f.PCM))
		chunk := audioChunk{pcm: bytes.Clone(f.PCM[off:end]), captured: captured + uint32(off/192), generation: a.generation}
		select {
		case a.queue <- chunk:
		default:
			select {
			case <-a.queue:
			default:
			}
			a.queue <- chunk
		}
	}
	a.signalLocked()
}

func (s *Server) SetAudioAvailable(available bool) {
	if available {
		return
	}
	s.mu.Lock()
	c := s.active
	s.mu.Unlock()
	if c == nil {
		return
	}
	a := &c.audio
	a.mu.Lock()
	defer a.mu.Unlock()
	a.generation++
	for {
		select {
		case <-a.queue:
		default:
			return
		}
	}
}

func (c *client) startAudioLoop() {
	a := &c.audio
	a.mu.Lock()
	if !a.started || a.loopStarted {
		a.mu.Unlock()
		return
	}
	a.loopStarted = true
	a.mu.Unlock()
	c.s.wg.Add(1)
	go func() {
		defer c.s.wg.Done()
		if err := c.audioLoop(); err != nil {
			log.Printf("rdp: audio transport: %v", err)
			c.close()
		}
	}()
}

func (c *client) audioLoop() error {
	a := &c.audio
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	var nextSend time.Time
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		var paced <-chan time.Time
		if delay := time.Until(nextSend); delay > 0 {
			timer.Reset(delay)
			paced = timer.C
		}
		select {
		case <-c.done:
			return nil
		case <-a.wake:
		case <-ticker.C:
		case <-paced:
		}
		now := time.Now()
		a.mu.Lock()
		if a.qualityPending && now.Sub(a.formatsAt) >= time.Second {
			a.qualityPending = false
			if err := c.sendAudioTrainingLocked(); err != nil {
				a.mu.Unlock()
				return err
			}
		}
		for block, pending := range a.outstanding {
			if now.Sub(pending.sent) >= 2*time.Second {
				// A receipt-ACKed block still occupies the playback device.
				// Expiring it would allow an unbounded client queue again.
				if a.playbackConfirm && pending.confirmed {
					continue
				}
				delete(a.outstanding, block)
				if !a.timeoutLogged {
					log.Printf("rdp: audio confirmation timed out; dropping stale audio")
					a.timeoutLogged = true
				}
			}
		}
		if a.trained && now.Sub(a.statsLogged) >= 5*time.Second {
			log.Printf("rdp: audio timing elapsed_ms=%d captured_ms=%d sent_ms=%d queued=%d unplayed_ms=%d confirms=%d playback_confirms=%d max_client_hold_ms=%d", now.Sub(a.statsStarted).Milliseconds(), a.capturedDuration.Milliseconds(), a.sentDuration.Milliseconds(), len(a.queue), a.playbackQueuedLocked().Milliseconds(), a.confirmCount, a.playbackCount, a.maxConfirmHold.Milliseconds())
			a.statsLogged, a.maxConfirmHold = now, 0
		}
		if !a.trained || len(a.outstanding) >= audioWindow || a.playbackQueuedLocked() >= audioPlayWindow || now.Before(nextSend) {
			a.mu.Unlock()
			continue
		}
		var chunk audioChunk
		select {
		case chunk = <-a.queue:
		default:
			a.mu.Unlock()
			continue
		}
		if chunk.generation != a.generation || int32(audioTicks(now)-chunk.captured) > int32(audioMaxAge/time.Millisecond) || (a.version < 8 && len(chunk.pcm) <= 4) {
			a.mu.Unlock()
			continue
		}
		a.mu.Unlock()
		sent, err := c.sendQueuedAudio(chunk)
		if err != nil {
			return err
		}
		if !sent.IsZero() {
			// Do not repay a scheduling/network stall by bursting old samples into
			// the client's playback queue. Pace from the actual transmission start.
			nextSend = sent.Add(time.Duration(len(chunk.pcm)) * time.Second / 192000)
		}
	}
}

// Recheck freshness after waiting behind graphics/clipboard traffic. Keep the
// legacy WaveInfo/Wave pair atomic on the static channel transport as well.
func (c *client) sendQueuedAudio(chunk audioChunk) (time.Time, error) {
	c.staticWriteMu.Lock()
	defer c.staticWriteMu.Unlock()
	a := &c.audio
	a.mu.Lock()
	now := time.Now()
	select {
	case <-c.done:
		a.mu.Unlock()
		return time.Time{}, nil
	default:
	}
	block := a.nextBlock
	_, busy := a.outstanding[block]
	if chunk.generation != a.generation || int32(audioTicks(now)-chunk.captured) > int32(audioMaxAge/time.Millisecond) || busy || a.playbackQueuedLocked()+time.Duration(len(chunk.pcm))*time.Second/192000 > audioPlayWindow {
		a.mu.Unlock()
		return time.Time{}, nil
	}
	stamp := uint16(audioTicks(now))
	a.nextBlock++
	// Retire the previous generation of this eight-bit block number.
	delete(a.received, block)
	a.outstanding[block] = audioPending{sent: now, stamp: stamp, duration: time.Duration(len(chunk.pcm)) * time.Second / 192000}
	a.sentDuration += time.Duration(len(chunk.pcm)) * time.Second / 192000
	version, format := a.version, a.format
	a.mu.Unlock()
	return now, c.sendAudioWaveLocked(chunk, version, format, block, stamp)
}

func (c *client) sendAudioWave(chunk audioChunk, version, format uint16, block byte, stamp uint16) error {
	c.staticWriteMu.Lock()
	defer c.staticWriteMu.Unlock()
	return c.sendAudioWaveLocked(chunk, version, format, block, stamp)
}

func (c *client) sendAudioWaveLocked(chunk audioChunk, version, format uint16, block byte, stamp uint16) error {
	header := join(u16(int(stamp)), u16(int(format)), []byte{block, 0, 0, 0})
	if version >= 8 {
		return c.sendStaticVCLocked(c.data.audio, audioPDU(soundWave2, join(header, u32(chunk.captured), chunk.pcm)))
	}
	// Older clients use two separate virtual-channel messages. WaveInfo carries
	// the first four PCM bytes; Wave replaces those bytes with four padding bytes.
	info := join([]byte{soundWave, 0}, u16(8+len(chunk.pcm)), header, chunk.pcm[:4])
	if err := c.sendStaticVCLocked(c.data.audio, info); err != nil {
		return err
	}
	return c.sendStaticVCLocked(c.data.audio, join(make([]byte, 4), chunk.pcm[4:]))
}

var _ bridge.AudioSink = (*Server)(nil)
