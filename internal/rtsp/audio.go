package rtsp

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"anykvm/internal/capture"
)

type audioTrack struct {
	control            string
	pt, rate, channels int
	codec              string
	config             []byte
	fragment           []byte
	expected           int
	seq                uint16
	stamp, origin      uint32
	started            bool
}
type audioDevice struct {
	d    *Device
	mu   sync.Mutex
	open bool
}

func (d *Device) AudioDevice() capture.AudioDevice { return &audioDevice{d: d} }
func (a *audioDevice) Open() error                 { a.mu.Lock(); a.open = true; a.mu.Unlock(); return nil }
func (a *audioDevice) Close() error                { a.mu.Lock(); a.open = false; a.mu.Unlock(); return nil }
func (a *audioDevice) Capture() (*capture.AudioFrame, error) {
	a.mu.Lock()
	open := a.open
	a.mu.Unlock()
	if !open {
		return nil, capture.ErrStreamStopped
	}
	a.d.mu.Lock()
	s := a.d.s
	a.d.mu.Unlock()
	if s == nil {
		return nil, nil
	}
	t := time.NewTimer(100 * time.Millisecond)
	defer t.Stop()
	select {
	case f := <-s.audio:
		return f, nil
	case <-s.done:
		return nil, capture.ErrStreamStopped
	case <-t.C:
		return nil, nil
	}
}
func resolveControl(base, ref string) (string, error) {
	// URL resolution is shared with SDP video parsing below.
	return controlURL(base, ref)
}
func sdpAudio(body []byte, base string) (*audioTrack, error) {
	var inAudio bool
	var control string
	maps := map[int][]string{}
	params := map[int]string{}
	var candidates []int
	for _, line := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "m=") {
			f := strings.Fields(line[2:])
			if inAudio {
				break
			}
			inAudio = len(f) >= 4 && f[0] == "audio"
			if inAudio {
				for _, pt := range f[3:] {
					n, e := strconv.Atoi(pt)
					if e == nil {
						candidates = append(candidates, n)
					}
				}
			}
		}
		if !inAudio {
			continue
		}
		if strings.HasPrefix(line, "a=control:") {
			control = strings.TrimPrefix(line, "a=control:")
		}
		if strings.HasPrefix(line, "a=rtpmap:") {
			f := strings.Fields(strings.TrimPrefix(line, "a=rtpmap:"))
			if len(f) == 2 {
				n, e := strconv.Atoi(f[0])
				if e == nil {
					maps[n] = strings.Split(f[1], "/")
				}
			}
		}
		if strings.HasPrefix(line, "a=fmtp:") {
			f := strings.SplitN(strings.TrimPrefix(line, "a=fmtp:"), " ", 2)
			if len(f) == 2 {
				n, e := strconv.Atoi(f[0])
				if e == nil {
					params[n] = f[1]
				}
			}
		}
	}
	for _, pt := range candidates {
		m := maps[pt]
		if len(m) == 0 {
			if pt == 0 {
				m = []string{"PCMU", "8000", "1"}
			}
			if pt == 8 {
				m = []string{"PCMA", "8000", "1"}
			}
		}
		if len(m) < 2 {
			continue
		}
		codec := strings.ToUpper(m[0])
		rate, e := strconv.Atoi(m[1])
		if e != nil {
			continue
		}
		channels := 1
		if len(m) > 2 {
			channels, _ = strconv.Atoi(m[2])
		}
		a := &audioTrack{pt: pt, rate: rate, channels: channels, codec: codec}
		switch codec {
		case "PCMU", "PCMA":
			if rate != 8000 || channels != 1 {
				continue
			}
		case "MPEG4-GENERIC":
			if rate < 8000 || rate > 96000 || channels < 1 || channels > 2 {
				continue
			}
			options := map[string]string{}
			for _, param := range strings.Split(params[pt], ";") {
				k, v, ok := strings.Cut(strings.TrimSpace(param), "=")
				if ok {
					options[strings.ToLower(k)] = v
				}
			}
			if !strings.EqualFold(options["mode"], "AAC-hbr") || options["sizelength"] != "13" || options["indexlength"] != "3" || options["indexdeltalength"] != "3" {
				continue
			}
			config, e := hex.DecodeString(options["config"])
			if e != nil || len(config) < 2 || config[0]>>3 != 2 {
				continue
			}
			a.config = config
		default:
			continue
		}
		var err error
		a.control, err = resolveControl(base, control)
		return a, err
	}
	return nil, nil // Unsupported or absent optional audio does not disable video.
}
func rtpPayload(p []byte) ([]byte, int, uint16, uint32, error) {
	if len(p) < 12 || p[0]>>6 != 2 {
		return nil, 0, 0, 0, fmt.Errorf("invalid RTP audio header")
	}
	off := 12 + int(p[0]&15)*4
	if off > len(p) {
		return nil, 0, 0, 0, fmt.Errorf("truncated RTP CSRC")
	}
	if p[0]&16 != 0 {
		if off+4 > len(p) {
			return nil, 0, 0, 0, fmt.Errorf("truncated RTP extension")
		}
		off += 4 + int(binary.BigEndian.Uint16(p[off+2:]))*4
	}
	end := len(p)
	if p[0]&32 != 0 {
		pad := int(p[end-1])
		if pad < 1 || pad > end-off {
			return nil, 0, 0, 0, fmt.Errorf("invalid RTP padding")
		}
		end -= pad
	}
	if off >= end {
		return nil, 0, 0, 0, fmt.Errorf("truncated RTP audio")
	}
	return p[off:end], int(p[1] & 127), binary.BigEndian.Uint16(p[2:]), binary.BigEndian.Uint32(p[4:]), nil
}
func (a *audioTrack) push(packet []byte) ([]*capture.AudioFrame, error) {
	p, pt, seq, stamp, e := rtpPayload(packet)
	if e != nil {
		return nil, e
	}
	if pt != a.pt {
		return nil, nil
	}
	if !a.started {
		a.origin = stamp
		a.started = true
	}
	if seq != a.seq+1 || stamp != a.stamp {
		a.fragment = nil
		a.expected = 0
	}
	a.seq, a.stamp = seq, stamp
	timestamp := int64(uint32(stamp-a.origin)) * 1000000 / int64(a.rate)
	if a.codec != "MPEG4-GENERIC" {
		pcm := make([]byte, len(p)*6*4)
		for i, v := range p {
			sample := g711(v, a.codec == "PCMA")
			for j := 0; j < 6; j++ {
				off := (i*6 + j) * 4
				binary.LittleEndian.PutUint16(pcm[off:], uint16(sample))
				binary.LittleEndian.PutUint16(pcm[off+2:], uint16(sample))
			}
		}
		return []*capture.AudioFrame{{PCM: pcm, SampleRate: 48000, Channels: 2, Timestamp: timestamp}}, nil
	}
	if len(p) < 4 {
		return nil, fmt.Errorf("truncated AAC RTP packet")
	}
	bits := int(binary.BigEndian.Uint16(p))
	if bits == 0 || bits%16 != 0 || 2+bits/8 > len(p) {
		return nil, fmt.Errorf("unsupported AAC AU headers")
	}
	count := bits / 16
	headers, data := p[2:2+bits/8], p[2+bits/8:]
	var frames []*capture.AudioFrame
	for i := 0; i < count; i++ {
		header := binary.BigEndian.Uint16(headers[i*2:])
		size := int(header >> 3)
		if header&7 != 0 || size == 0 {
			return nil, fmt.Errorf("unsupported AAC AU index")
		}
		if count == 1 && size > len(data) {
			if a.expected != 0 && a.expected != size {
				a.fragment = nil
			}
			a.expected = size
			a.fragment = append(a.fragment, data...)
			if len(a.fragment) < size {
				return nil, nil
			}
			if len(a.fragment) != size {
				return nil, fmt.Errorf("invalid AAC fragment size")
			}
			data = a.fragment
			a.fragment = nil
			a.expected = 0
		}
		if size > len(data) {
			return nil, fmt.Errorf("truncated AAC access unit")
		}
		frames = append(frames, &capture.AudioFrame{Encoded: append([]byte(nil), data[:size]...), Codec: "mp4a.40.2", Config: append([]byte(nil), a.config...), SampleRate: a.rate, Channels: a.channels, FramesPerPacket: 1024, Timestamp: timestamp + int64(i*1024)*1000000/int64(a.rate)})
		data = data[size:]
	}
	if len(data) != 0 {
		return nil, fmt.Errorf("trailing AAC RTP bytes")
	}
	return frames, nil
}
func g711(v byte, alaw bool) int16 {
	if alaw {
		v ^= 0x55
		sample := int(v&15) << 4
		seg := int(v >> 4 & 7)
		if seg == 0 {
			sample += 8
		} else {
			sample += 0x108
			sample <<= seg - 1
		}
		if v&0x80 == 0 {
			sample = -sample
		}
		return int16(sample)
	}
	v = ^v
	sample := ((int(v&15) << 3) + 0x84) << uint(v>>4&7)
	sample -= 0x84
	if v&0x80 != 0 {
		sample = -sample
	}
	return int16(sample)
}
