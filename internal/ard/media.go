package ard

import (
	"fmt"
	"log"
	"net"
	"time"

	"anykvm/internal/avmedia"
	"anykvm/internal/capture"
)

func (s *session) bindPorts(body []byte) error {
	if len(body) < 24 {
		return fmt.Errorf("ard: truncated media ports")
	}
	peer, ok := s.conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return fmt.Errorf("ard: no numeric media peer")
	}
	s.udpMu.Lock()
	defer s.udpMu.Unlock()
	// Verified native port slots plus the older two-byte-shifted variant.
	for _, off := range []int{8, 14, 20, 10, 16, 22} {
		port := int(be16(body[off:]))
		if port < 1024 {
			continue
		}
		if _, ok := s.udp[port]; ok {
			continue
		}
		select {
		case <-s.done:
			return capture.ErrStreamStopped
		default:
		}
		network := "udp4"
		if peer.IP.To4() == nil {
			network = "udp6"
		}
		local, ok := s.conn.LocalAddr().(*net.TCPAddr)
		if !ok {
			return fmt.Errorf("ard: no numeric local media endpoint")
		}
		dialer := net.Dialer{LocalAddr: &net.UDPAddr{IP: local.IP, Port: port, Zone: local.Zone}, Timeout: time.Second, Control: reuseMediaPort}
		conn, err := dialer.Dial(network, (&net.UDPAddr{IP: peer.IP, Port: port, Zone: peer.Zone}).String())
		if err != nil {
			return fmt.Errorf("%w: cannot bind symmetric media UDP port %d: %v", ErrUnsupported, port, err)
		}
		u := conn.(*net.UDPConn)
		u.SetReadBuffer(4 << 20)
		s.udp[port] = u
		s.workers.Add(1)
		go s.receiveDatagrams(u)
	}
	if len(s.udp) == 0 {
		return fmt.Errorf("%w: no usable media UDP ports", ErrUnsupported)
	}
	return nil
}
func (s *session) receiveDatagrams(u *net.UDPConn) {
	defer s.workers.Done()
	buf := make([]byte, 65535)
	for {
		n, err := u.Read(buf)
		if err != nil {
			select {
			case <-s.done:
			default:
				s.stop(fmt.Errorf("ard: UDP media receive: %w", err))
			}
			return
		}
		p := append([]byte(nil), buf[:n]...)
		select {
		case s.datagrams <- p:
		case <-s.done:
			return
		default:
		}
	}
}

func (s *session) mediaLoop() {
	defer s.workers.Done()
	var decoder *avmedia.Session
	defer func() {
		if decoder != nil {
			decoder.Close()
		}
	}()
	assembler := hevcAssembler{waiting: true}
	reorder := rtpReorder{}
	var generation uint64
	var origin time.Time
	var firstStamp uint32
	var lastInfo hevcInfo
	var lastPacket time.Time
	lastFeedback := time.Time{}
	fir := byte(0)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	lastRestart := time.Now()
	// Datagram buffering is bounded even when media races the negotiated keys.
	var beforeKeys [][]byte
	process := func(p []byte) error {
		au, stamp, recovery, err := assembler.feed(p)
		if err != nil {
			assembler.loss()
			return nil
		}
		if len(au) == 0 {
			return nil
		}
		if recovery || decoder == nil {
			info, parseErr := parseSPS(assembler.sets[1])
			if parseErr != nil {
				assembler.loss()
				return nil
			}
			if info != lastInfo {
				log.Printf("ard: HEVC SPS profile=%d level=%d chroma_idc=%d bit_depth=%d/%d picture=%dx%d decoder=libav software", info.Profile, info.Level, info.Chroma, info.DepthLuma, info.DepthChroma, info.Width, info.Height)
				lastInfo = info
			}
			if info.Width*info.Height > maxSinglePicturePixels {
				return fmt.Errorf("%w: compound HEVC desktop is unsupported", ErrUnsupported)
			}
			if decoder != nil {
				decoder.Close()
			}
			decoder, err = avmedia.NewDecoder("hevc")
			if err != nil {
				return fmt.Errorf("%w: HEVC decoder: %v", ErrUnsupported, err)
			}
		}
		frame, err := decoder.Decode(au)
		if err != nil {
			assembler.loss()
			decoder.Close()
			decoder = nil
			return nil
		}
		if frame == nil {
			return nil
		}
		s.mu.Lock()
		w, h := s.w, s.h
		if w == 0 && h == 0 && validSize(frame.Width, frame.Height) {
			w, h = frame.Width, frame.Height
			s.w, s.h = w, h
		}
		s.mu.Unlock()
		if frame.Width != w || frame.Height != h {
			return fmt.Errorf("%w: HEVC picture %dx%d differs from desktop %dx%d; region composition is required", ErrUnsupported, frame.Width, frame.Height, w, h)
		}
		if !validSize(frame.Width, frame.Height) || len(frame.Data) != frame.Width*frame.Height*4 {
			return fmt.Errorf("ard: invalid decoded picture")
		}
		if origin.IsZero() {
			origin = time.Now()
			firstStamp = stamp
		}
		// Single-picture screen RTP uses a 90kHz clock. Clock resets accompany
		// media generations; uint32 subtraction preserves timestamp wraparound.
		present := origin.Add(time.Duration(uint32(stamp-firstStamp)) * time.Second / 90000)
		s.mediaMu.Lock()
		if s.generation == generation {
			s.emit(&capture.Frame{Format: capture.FormatRaw, Data: frame.Data, Width: frame.Width, Height: frame.Height, SourceWidth: frame.Width, SourceHeight: frame.Height, CapturedAt: present})
		}
		s.mediaMu.Unlock()
		return nil
	}
	deliver := func(packets [][]byte, gap bool) error {
		if gap {
			assembler.loss()
		}
		for _, p := range packets {
			if err := process(p); err != nil {
				return err
			}
		}
		return nil
	}
	ingest := func(batch [][]byte) error {
		s.mediaMu.Lock()
		receiver, gen := s.receiver, s.generation
		s.mediaMu.Unlock()
		if receiver == nil {
			for _, p := range batch {
				if len(beforeKeys) < 256 {
					beforeKeys = append(beforeKeys, p)
				}
			}
			return nil
		}
		if gen != generation {
			generation = gen
			assembler = hevcAssembler{waiting: true}
			reorder = rtpReorder{}
			origin = time.Time{}
			if decoder != nil {
				decoder.Close()
				decoder = nil
			}
		}
		batch = append(beforeKeys, batch...)
		beforeKeys = nil
		for _, encrypted := range batch {
			// Audio and SRTCP have different keys/framing; only PT 100 is screen HEVC.
			if len(encrypted) < 12 || encrypted[1]&127 != 100 {
				continue
			}
			rtp, err := receiver.unprotect(encrypted)
			if err != nil {
				continue
			}
			lastPacket = time.Now()
			packets, gap := reorder.push(rtp, time.Now())
			if err = deliver(packets, gap); err != nil {
				return err
			}
		}
		return nil
	}
	for {
		select {
		case <-s.done:
			return
		case p := <-s.datagrams:
			if err := ingest([][]byte{p}); err != nil {
				s.stop(err)
				return
			}
		case now := <-tick.C:
			if err := ingest(nil); err != nil {
				s.stop(err)
				return
			}
			if assembler.active && !lastPacket.IsZero() && now.Sub(lastPacket) > 250*time.Millisecond {
				assembler.loss()
			}
			s.mu.Lock()
			s.mediaWaiting = assembler.waiting
			s.mu.Unlock()
			packets, gap := reorder.flush(now)
			if err := deliver(packets, gap); err != nil {
				s.stop(err)
				return
			}
			if now.Sub(lastFeedback) >= time.Second {
				s.mediaMu.Lock()
				feedback, receiver, local := s.feedback, s.receiver, s.localSSRC
				var packet []byte
				if feedback != nil && receiver != nil && receiver.initialized {
					// Empty Receiver Report keeps the negotiated receiver alive.
					packet = make([]byte, 8)
					packet[0], packet[1] = 128, 201
					put16(packet[2:], 1)
					put32(packet[4:], local)
					if assembler.waiting {
						fir++
						p := make([]byte, 20)
						p[0], p[1] = 132, 206
						put16(p[2:], 4)
						put32(p[4:], local)
						put32(p[12:], receiver.ssrc)
						p[16] = fir
						packet = append(packet, p...)
					}
					packet = feedback.protect(packet)
				}
				s.mediaMu.Unlock()
				if len(packet) > 0 {
					s.udpMu.Lock()
					for _, u := range s.udp {
						u.SetWriteDeadline(now.Add(100 * time.Millisecond))
						u.Write(packet)
					}
					s.udpMu.Unlock()
				}
				lastFeedback = now
			}
			if assembler.waiting && now.Sub(lastRestart) > 5*time.Second {
				if err := s.write(mediaRequest()); err != nil {
					s.stop(err)
					return
				}
				log.Printf("ard: requesting media restart after lost HEVC reference state")
				lastRestart = now
			}
		}
	}
}
