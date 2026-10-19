package airplay

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha512"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"net"
	"sync"
	"time"

	"anykvm/internal/capture"
)

// Legacy paired type-96 audio uses AAC-ELD for mirroring and ALAC for
// standalone playback. Both formats are 44.1 kHz stereo; buffered audio is off.
const mirrorAudioFormat uint64 = 1 << 24
const alacAudioFormat uint64 = 1 << 18
const supportedAudioFormats = mirrorAudioFormat | alacAudioFormat

type audioOptions struct {
	CT          uint64 `plist:"ct"`
	SPF         uint64 `plist:"spf"`
	Format      uint64 `plist:"audioFormat"`
	ControlPort uint64 `plist:"controlPort"`
	SampleRate  uint64 `plist:"sr"`
}

func (o audioOptions) normalized() (audioOptions, error) {
	if o.CT == 0 {
		o.CT = 8
		if o.Format == alacAudioFormat {
			o.CT = 2
		}
	}
	spf, format := uint64(480), mirrorAudioFormat
	if o.CT == 2 {
		spf, format = 352, alacAudioFormat
	}
	if o.SPF == 0 {
		o.SPF = spf
	}
	if o.Format == 0 {
		o.Format = format
	}
	if o.SampleRate == 0 {
		o.SampleRate = 44100
	}
	if o.CT != 8 && o.CT != 2 || o.SPF != spf || o.Format != format || o.SampleRate != 44100 || o.ControlPort > 65535 {
		return o, fmt.Errorf("airplay: unsupported audio ct=%d spf=%d format=0x%x sr=%d", o.CT, o.SPF, o.Format, o.SampleRate)
	}
	return o, nil
}

var mirrorAudioConfig = []byte{0xf8, 0xe8, 0x50, 0} // AAC-ELD, 44100Hz stereo, 480 samples

// Big-endian ALACSpecificConfig: 352 frames, 16-bit stereo, 44100 Hz.
var alacAudioConfig = []byte{0, 0, 1, 0x60, 0, 16, 40, 10, 14, 2, 0, 255, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xac, 0x44}

func (o audioOptions) frameFormat() capture.AudioFrame {
	f := capture.AudioFrame{Codec: "mp4a.40.39", Config: mirrorAudioConfig,
		SampleRate: 44100, Channels: 2, FramesPerPacket: int(o.SPF)}
	if o.CT == 2 {
		f.Codec, f.Config = "alac", alacAudioConfig
	}
	return f
}

type audioStream struct {
	media         *mediaSession
	options       audioOptions
	data, control *net.UDPConn
	block         cipher.Block
	iv            []byte
	done          chan struct{}
	mu            sync.Mutex // serialize FLUSH/volume and data/control packet processing
	queue         audioPacketQueue
	stampSet      bool
	lastStamp     uint32
	ticks         int64
	volume        float64
	controlSeq    uint16
	lastResend    time.Time
	markerLogged  bool
	packetLogged  bool
}

func newAudioStream(s *mediaSession, options audioOptions, data, control *net.UDPConn) (*audioStream, error) {
	if len(s.key) != 16 || len(s.secret) != 32 || len(s.audioIV) != 16 {
		return nil, fmt.Errorf("airplay: invalid audio key/secret/IV")
	}
	h := sha512.New()
	h.Write(s.key)
	h.Write(s.secret)
	block, err := aes.NewCipher(h.Sum(nil)[:16])
	if err != nil {
		return nil, err
	}
	return &audioStream{media: s, options: options, data: data, control: control, block: block, iv: append([]byte(nil), s.audioIV...), done: make(chan struct{}), volume: audioGain(s.audioVolumeDB)}, nil
}

// Bound packet reordering to 32 packets and 30 ms. Sequence comparisons handle
// wraparound; iOS sends redundant copies which must never be decoded twice.
type audioPacketQueue struct {
	started bool
	next    uint16
	pending map[uint16][]byte
	gap     time.Time
}

func (q *audioPacketQueue) push(seq uint16, payload []byte, now time.Time) {
	if !q.started {
		q.started, q.next, q.pending = true, seq, make(map[uint16][]byte)
	}
	distance := int16(seq - q.next)
	if distance < 0 {
		return
	}
	if distance >= 32 {
		q.next, q.pending, q.gap = seq, make(map[uint16][]byte), time.Time{}
	}
	if _, duplicate := q.pending[seq]; !duplicate {
		q.pending[seq] = append([]byte(nil), payload...)
	}
	if seq != q.next && q.gap.IsZero() {
		q.gap = now
	}
}

func (q *audioPacketQueue) pop(now time.Time) []byte {
	if len(q.pending) == 0 {
		q.gap = time.Time{}
		return nil
	}
	if _, ok := q.pending[q.next]; !ok {
		if q.gap.IsZero() {
			q.gap = now
		}
		if now.Sub(q.gap) < 30*time.Millisecond {
			return nil
		}
		for i := 0; i < 32; i++ {
			q.next++
			if _, ok := q.pending[q.next]; ok {
				break
			}
		}
	}
	payload := q.pending[q.next]
	delete(q.pending, q.next)
	q.next++
	q.gap = time.Time{}
	return payload
}

func (a *audioStream) run() {
	defer close(a.done)
	var workers sync.WaitGroup
	for _, conn := range []*net.UDPConn{a.data, a.control} {
		workers.Add(1)
		go func(conn *net.UDPConn) {
			defer workers.Done()
			var packet [2048]byte
			for {
				conn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
				n, peer, err := conn.ReadFromUDP(packet[:])
				if err != nil {
					if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
						a.mu.Lock()
						a.drain(time.Now())
						a.mu.Unlock()
						continue
					}
					return
				}
				if !peer.IP.Equal(a.media.peerIP) {
					continue
				}
				p := packet[:n]
				if conn == a.control {
					if n < 4 || p[1]&0x7f != 0x56 {
						continue
					}
					p = p[4:] // retransmitted RTP packet
				}
				a.mu.Lock()
				a.receivePacket(p, time.Now())
				a.mu.Unlock()
			}
		}(conn)
	}
	workers.Wait()
}

func (a *audioStream) receivePacket(p []byte, now time.Time) {
	// AirPlay uses a fixed 12-byte RTP header, without CSRCs/extensions.
	if len(p) <= 12 || len(p) > 2048 || p[0] != 0x80 || p[1]&0x7f != 96 {
		return
	}
	payload := p[12:]
	if bytes.Equal(payload, []byte{0, 0x68, 0x34, 0}) {
		if !a.markerLogged {
			log.Printf("airplay: audio UDP received no-data marker; waiting for sender audio")
			a.markerLogged = true
		}
		return
	} // initial no-data marker
	if !a.packetLogged {
		r := a.media.receiver
		r.mu.Lock()
		attached := r.audioOpen && r.owner == a.media.owner
		r.mu.Unlock()
		log.Printf("airplay: audio UDP received codec=%s (%d bytes, viewer attached=%t)", a.options.frameFormat().Codec, len(payload), attached)
		a.packetLogged = true
	}
	// Retain RTP timestamp through reordering; the first 8 bytes precede the AU.
	a.queue.push(binary.BigEndian.Uint16(p[2:4]), p[4:], now)
	a.drain(now)
}

func (a *audioStream) drain(now time.Time) {
	for {
		payload := a.queue.pop(now)
		if payload == nil {
			break
		}
		if len(payload) <= 8 {
			continue
		}
		stamp := binary.BigEndian.Uint32(payload)
		if a.stampSet {
			a.ticks += int64(int32(stamp - a.lastStamp))
		} else {
			a.stampSet = true
		}
		a.lastStamp = stamp
		payload = payload[8:]
		// Each packet resets CBC to SETUP's IV. Only full AES blocks are
		// encrypted; the trailing bytes are plaintext and carry no padding.
		plain := append([]byte(nil), payload...)
		encrypted := len(plain) / aes.BlockSize * aes.BlockSize
		cipher.NewCBCDecrypter(a.block, a.iv).CryptBlocks(plain[:encrypted], payload[:encrypted])
		gain := a.volume
		frame := a.options.frameFormat()
		frame.Encoded, frame.Timestamp, frame.Gain = plain, a.ticks*1000000/44100, &gain
		a.media.receiver.pushAudio(a.media.owner, frame)
	}
	if len(a.queue.pending) != 0 && a.options.ControlPort != 0 && now.Sub(a.lastResend) >= 10*time.Millisecond {
		var request [8]byte
		request[0], request[1] = 0x80, 0xd5
		binary.BigEndian.PutUint16(request[2:4], a.controlSeq)
		binary.BigEndian.PutUint16(request[4:6], a.queue.next)
		binary.BigEndian.PutUint16(request[6:8], 1)
		a.controlSeq++
		a.lastResend = now
		a.control.WriteToUDP(request[:], &net.UDPAddr{IP: a.media.peerIP, Port: int(a.options.ControlPort), Zone: a.media.zone})
	}
}

func (s *mediaSession) flushAudio(next *uint16) {
	s.mu.Lock()
	a := s.audio
	s.mu.Unlock()
	if a != nil {
		a.mu.Lock()
		a.queue = audioPacketQueue{}
		if next != nil {
			a.queue = audioPacketQueue{started: true, next: *next, pending: make(map[uint16][]byte)}
		}
		a.stampSet = false
		a.ticks = 0
		s.receiver.clearSessionAudio(s.owner)
		a.mu.Unlock()
	}
}

func (s *mediaSession) setAudioVolume(db float64) {
	if math.IsNaN(db) || math.IsInf(db, 0) {
		return
	}
	gain := audioGain(db)
	s.mu.Lock()
	s.audioVolumeDB = db
	a := s.audio
	s.mu.Unlock()
	if a != nil {
		a.mu.Lock()
		a.volume = gain
		a.mu.Unlock()
	}
}

func audioGain(db float64) float64 {
	if db <= -144 {
		return 0
	}
	return math.Pow(10, max(-30, min(0, db))/20)
}

// Audio capture attaches to the receiver's existing pairing and sockets.
// Closing a viewer detaches audio without stopping discovery or video.
type audioDevice struct{ receiver *Receiver }

func (d *Device) AudioDevice() capture.AudioDevice { return &audioDevice{d.receiver} }

func (d *audioDevice) AudioAvailable() bool {
	r := d.receiver
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.audioOpen && r.listener != nil && r.audioOwner != nil && r.audioOwner == r.owner
}

func (d *audioDevice) Open() error {
	r := d.receiver
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listener == nil {
		return capture.ErrStreamStopped
	}
	r.audioOpen = true
	r.clearAudioLocked()
	return nil
}

func (d *audioDevice) Capture() (*capture.AudioFrame, error) {
	r := d.receiver
	r.mu.Lock()
	if !r.audioOpen {
		r.mu.Unlock()
		return nil, capture.ErrStreamStopped
	}
	frames, stop, epoch := r.audioFrames, r.stop, r.audioEpoch
	r.mu.Unlock()
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-stop:
		return nil, capture.ErrStreamStopped
	case <-timer.C:
		return nil, nil
	case frame := <-frames:
		r.mu.Lock()
		active := r.audioOpen && r.audioEpoch == epoch
		r.mu.Unlock()
		if !active {
			return nil, nil
		}
		return &frame, nil
	}
}

func (d *audioDevice) Close() error {
	r := d.receiver
	r.mu.Lock()
	r.audioOpen = false
	r.clearAudioLocked()
	r.mu.Unlock()
	return nil
}

func (r *Receiver) clearAudioLocked() {
	r.audioEpoch++
	for {
		select {
		case <-r.audioFrames:
		default:
			return
		}
	}
}
func (r *Receiver) clearSessionAudio(owner *pairing) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Retained RTP controls may close or flush after another session starts.
	// Their cleanup must not discard the new sender's audio or change its epoch.
	if r.owner == owner {
		r.clearAudioLocked()
	}
}

func (r *Receiver) pushAudio(owner *pairing, frame capture.AudioFrame) {
	if len(frame.Encoded) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.audioOpen || r.owner != owner {
		return
	}
	frame.Epoch = r.audioEpoch
	select {
	case r.audioFrames <- frame:
	default:
		// Drop old access units when a viewer lags; never block the phone or video.
		select {
		case <-r.audioFrames:
		default:
		}
		select {
		case r.audioFrames <- frame:
		default:
		}
	}
}

var _ capture.AudioDevice = (*audioDevice)(nil)
