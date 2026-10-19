package coredevice

import (
	"anykvm/internal/capture"
	"context"
	"encoding/binary"
	"fmt"
	"github.com/google/uuid"
	"log"
	"net"
	"sync"
	"time"
)

var aacELDConfig = []byte{0xf8, 0xe6, 0x50, 0x00} // AAC-ELD, 48kHz stereo, 480 samples
var aacELD512Config = []byte{0xf8, 0xe6, 0x40, 0x00}

func (c *Client) AudioDevice() capture.AudioDevice {
	if c.inputOnly {
		return nil
	}
	c.wantAudio.Store(true)
	return &coreAudioDevice{client: c}
}

type coreAudioDevice struct {
	mu     sync.Mutex
	client *Client
	stream *audioStream
}

func (d *coreAudioDevice) Open() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stream != nil {
		return nil
	}
	d.client.mu.Lock()
	s := d.client.session
	d.client.mu.Unlock()
	if s == nil {
		return capture.ErrStreamStopped
	}
	if s.audioErr != nil {
		return s.audioErr
	}
	if s.audio == nil {
		return fmt.Errorf("coredevice: audio not negotiated")
	}
	s.audio.mu.Lock()
	defer s.audio.mu.Unlock()
	if s.audio.closed {
		return capture.ErrStreamStopped
	}
	if s.audio.active {
		return fmt.Errorf("coredevice: audio already subscribed")
	}
	s.audio.active = true
	s.audio.frames = nil
	d.stream = s.audio
	return nil
}
func (d *coreAudioDevice) Capture() (*capture.AudioFrame, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stream == nil {
		return nil, capture.ErrStreamStopped
	}
	s := d.stream
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, capture.ErrStreamStopped
	}
	if len(s.frames) == 0 {
		return nil, nil
	}
	f := s.frames[0]
	s.frames[0] = nil
	s.frames = s.frames[1:]
	return f, nil
}
func (d *coreAudioDevice) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s := d.stream; s != nil {
		s.mu.Lock()
		s.active = false
		s.frames = nil
		s.mu.Unlock()
		d.stream = nil
	}
	return nil
}

type audioStream struct {
	udp            *net.UDPConn
	channel        *xpcChannel
	peer           *net.UDPAddr
	config         streamConfig
	mu             sync.Mutex
	active, closed bool
	frames         []*capture.AudioFrame
	done           chan struct{}
	stopOnce       sync.Once
}

func startAudio(ctx context.Context, e *endpoint, id uuid.UUID) (*audioStream, error) {
	u, err := openReceiver(ctx, e.address)
	if err != nil {
		return nil, err
	}
	channel, err := e.connect(ctx, displayService)
	if err != nil {
		u.Close()
		return nil, err
	}
	// Use a separate XPC connection: an optional audio failure must not close video.
	audioCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cfg, err := channel.startAudioStream(audioCtx, e, u, id)
	if err != nil {
		channel.Close()
		u.Close()
		return nil, err
	}
	peer, err := net.ResolveUDPAddr("udp6", net.JoinHostPort(e.address, fmt.Sprint(cfg.sourcePort)))
	if err != nil {
		channel.Close()
		u.Close()
		return nil, err
	}
	channel.live()
	s := &audioStream{udp: u, channel: channel, peer: peer, config: cfg, done: make(chan struct{})}
	log.Printf("coredevice: native audio AAC-ELD 48000Hz stereo, 480 samples/packet PT=%d", cfg.payloadType)
	go s.loop()
	return s, nil
}
func (s *audioStream) stop() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.frames = nil
		s.mu.Unlock()
		s.udp.Close()
		if s.channel != nil {
			s.channel.Close()
		}
	})
}
func audioRTP(p []byte, cfg streamConfig) ([]byte, error) {
	h, err := rtpHeaderLen(p)
	if err != nil {
		return nil, err
	}
	if p[1]&127 != cfg.payloadType || be32(p[8:]) != cfg.remoteSSRC {
		return nil, fmt.Errorf("coredevice: unrelated audio RTP")
	}
	end := len(p)
	if p[0]&32 != 0 {
		n := int(p[end-1])
		if n == 0 || n > end-h {
			return nil, fmt.Errorf("coredevice: invalid audio padding")
		}
		end -= n
	}
	if end <= h {
		return nil, fmt.Errorf("coredevice: empty audio RTP")
	}
	return p[h:end], nil
}
func (s *audioStream) loop() {
	defer close(s.done)
	defer s.stop()
	buf := make([]byte, 65536)
	reorder := rtpReorder{}
	var highest uint32
	var lastSeq uint16
	var seqSet bool
	var lastStamp uint32
	var ticks int64
	var stampSet bool
	frameSamples := 480
	var first *capture.AudioFrame
	lastFeedback := time.Time{}
	logged := false
	emit := func(p []byte) {
		payload, err := audioRTP(p, s.config)
		if err != nil {
			return
		}
		seq := binary.BigEndian.Uint16(p[2:])
		contiguous := seqSet && uint16(seq-lastSeq) == 1
		if !seqSet {
			highest = uint32(seq)
			seqSet = true
		} else {
			highest += uint32(uint16(seq - lastSeq))
		}
		lastSeq = seq
		ts := binary.BigEndian.Uint32(p[4:])
		firstPacket := !stampSet
		if stampSet {
			step := int32(ts - lastStamp)
			if contiguous && (step == 480 || step == 512) && frameSamples != int(step) {
				frameSamples = int(step)
				log.Printf("coredevice: AAC-ELD packet duration=%d samples", frameSamples)
			}
			ticks += int64(step)
		} else {
			stampSet = true
		}
		lastStamp = ts
		if !logged {
			log.Printf("coredevice: first AAC-ELD packet (%d bytes)", len(payload))
			logged = true
		}
		config := aacELDConfig
		if frameSamples == 512 {
			config = aacELD512Config
		}
		f := &capture.AudioFrame{Encoded: payload, Codec: "mp4a.40.39", Config: config, SampleRate: 48000, Channels: 2, FramesPerPacket: frameSamples, Timestamp: ticks * 1000000 / 48000}
		// The second timestamp distinguishes 480/512-sample encoders before
		// sending an AAC configuration to the browser's decoder.
		if firstPacket {
			first = f
			return
		}
		s.mu.Lock()
		if s.active && !s.closed {
			if first != nil {
				first.Config = config
				first.FramesPerPacket = frameSamples
				s.frames = append(s.frames, first)
			}
			if len(s.frames) >= 16 {
				s.frames[0] = nil
				s.frames = s.frames[1:]
			}
			s.frames = append(s.frames, f)
		}
		first = nil
		s.mu.Unlock()
	}
	for {
		_ = s.udp.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
		n, peer, err := s.udp.ReadFromUDP(buf)
		now := time.Now()
		if err != nil {
			if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
				return
			}
		}
		if err == nil && peer.IP.Equal(s.peer.IP) {
			p := buf[:n]
			if _, valid := audioRTP(p, s.config); valid == nil {
				packets, _ := reorder.push(append([]byte(nil), p...), now)
				for _, p := range packets {
					emit(p)
				}
			}
		}
		packets, _ := reorder.flush(now)
		for _, p := range packets {
			emit(p)
		}
		if now.Sub(lastFeedback) >= time.Second {
			_ = s.udp.SetWriteDeadline(now.Add(time.Second))
			_, _ = s.udp.WriteToUDP(s.config.feedback(highest, false), s.peer)
			lastFeedback = now
		}
	}
}
