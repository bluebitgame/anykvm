package rdp

import (
	"bytes"
	"fmt"
	"sync"
	"time"

	"anykvm/internal/capture"
)

type sourceAudioDevice struct {
	c    *SourceClient
	mu   sync.Mutex
	open bool
}

func (c *SourceClient) AudioDevice() capture.AudioDevice { return &sourceAudioDevice{c: c} }
func (a *sourceAudioDevice) Open() error                 { a.mu.Lock(); a.open = true; a.mu.Unlock(); return nil }
func (a *sourceAudioDevice) Close() error                { a.mu.Lock(); a.open = false; a.mu.Unlock(); return nil }
func (a *sourceAudioDevice) Capture() (*capture.AudioFrame, error) {
	a.mu.Lock()
	open := a.open
	a.mu.Unlock()
	if !open {
		return nil, capture.ErrStreamStopped
	}
	s := a.c.current()
	if s == nil {
		return nil, nil
	}
	t := time.NewTimer(100 * time.Millisecond)
	defer t.Stop()
	select {
	case f := <-s.audio:
		return f, nil
	case <-s.done:
		return nil, s.failure()
	case <-t.C:
		return nil, nil
	}
}
func (s *sourceSession) handleSourceAudio(p []byte) error {
	// The original SNDC_WAVE carries its sample bytes in a second channel PDU,
	// whose first four bytes are padding, replaced by the WaveInfo prefix.
	if s.wavePending != nil {
		if len(p) != s.waveSize || len(p) < 4 {
			return errProtocol
		}
		pcm := append([]byte(nil), p...)
		copy(pcm, s.wavePending)
		s.wavePending = nil
		return s.sourcePCM(pcm, s.waveStamp, s.waveBlock)
	}
	if len(p) < 4 || int(le.Uint16(p[2:])) != len(p)-4 {
		return errProtocol
	}
	b := p[4:]
	switch p[0] {
	case soundFormats:
		if len(b) < 20 {
			return errProtocol
		}
		count, version := int(le.Uint16(b[14:])), le.Uint16(b[17:])
		if count > 64 || version < 2 {
			return fmt.Errorf("unsupported RDP audio formats")
		}
		s.audioFormat = -1
		formats := b[20:]
		for i := 0; i < count; i++ {
			if len(formats) < 18 {
				return errProtocol
			}
			n := 18 + int(le.Uint16(formats[16:]))
			if n > len(formats) {
				return errProtocol
			}
			if bytes.Equal(formats[:n], pcmAudioFormat()) && s.audioFormat < 0 {
				s.audioFormat = i
			}
			formats = formats[n:]
		}
		if len(formats) != 0 {
			return errProtocol
		}
		s.audioFormats = true
		body := join(u32(3), u32(0xffffffff), u32(0), u16(0), u16(0), []byte{0}, u16(int(min(version, uint16(soundVersion)))), []byte{0})
		if s.audioFormat >= 0 {
			le.PutUint16(body[14:], 1)
			body = append(body, pcmAudioFormat()...)
		}
		if e := s.sendVC(s.audioChannel, audioPDU(soundFormats, body)); e != nil {
			return e
		}
		if version >= 6 {
			return s.sendVC(s.audioChannel, audioPDU(soundQuality, join(u16(2), u16(0))))
		}
	case soundTraining:
		if len(b) < 4 {
			return errProtocol
		}
		return s.sendVC(s.audioChannel, audioPDU(soundTraining, join(b[:2], u16(0))))
	case soundWave2:
		if len(b) < 12 || !s.audioFormats || s.audioFormat < 0 || le.Uint16(b[2:]) != 0 {
			return errProtocol
		}
		return s.sourcePCM(b[12:], le.Uint16(b), b[4])
	case soundWave:
		// BodySize includes the separately transmitted sample bytes, hence this
		// legacy envelope is parsed specially by handleSourceVC before this method.
		return fmt.Errorf("invalid RDP legacy audio wave envelope")
	case 1: // SNDC_CLOSE
	case 3, 4: // Volume and pitch do not alter the PCM sample format.
	default:
		return fmt.Errorf("unsupported RDP audio command %d", p[0])
	}
	return nil
}
func (s *sourceSession) sourcePCM(p []byte, stamp uint16, block byte) error {
	if len(p) == 0 || len(p)%4 != 0 || len(p) > 65536 {
		return errProtocol
	}
	for len(p) > 0 {
		n := min(len(p), audioChunkBytes)
		f := &capture.AudioFrame{PCM: append([]byte(nil), p[:n]...), SampleRate: 48000, Channels: 2, Epoch: s.epoch}
		select {
		case s.audio <- f:
		default:
			select {
			case <-s.audio:
			default:
			}
			select {
			case s.audio <- f:
			default:
			}
		}
		p = p[n:]
	}
	return s.sendVC(s.audioChannel, audioPDU(soundConfirm, join(u16(int(stamp)), []byte{block, 0})))
}
