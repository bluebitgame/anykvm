package airplay

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha512"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"anykvm/internal/capture"
	"anykvm/internal/logging"
)

type mediaSession struct {
	receiver        *Receiver
	owner           *pairing
	key, secret     []byte
	audioIV         []byte
	audioVolumeDB   float64
	localIP, peerIP net.IP
	zone            string
	timing          *timingSession
	mu              sync.Mutex
	listener        net.Listener
	conn            net.Conn
	mirrorStop      chan struct{}
	mirrorDone      chan struct{}
	audioData       *net.UDPConn
	audioControl    *net.UDPConn
	audio           *audioStream
	connectionID    uint64
	stop            chan struct{}
	wg              sync.WaitGroup
}

func (s *mediaSession) stopAudio() {
	s.mu.Lock()
	data, control, audio := s.audioData, s.audioControl, s.audio
	s.audioData, s.audioControl, s.audio = nil, nil, nil
	s.mu.Unlock()
	if data != nil {
		data.Close()
		control.Close()
	}
	if audio != nil {
		<-audio.done
	}
	s.receiver.mu.Lock()
	if s.receiver.audioOwner == s.owner {
		s.receiver.audioOwner = nil
	}
	s.receiver.mu.Unlock()
	s.receiver.clearSessionAudio(s.owner)
}

func (s *mediaSession) startAudio(options audioOptions) (int, int, error) {
	options, err := options.normalized()
	if err != nil {
		return 0, 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.audioData != nil {
		if s.audio.options != options {
			return 0, 0, fmt.Errorf("airplay: audio format changed without teardown")
		}
		return s.audioData.LocalAddr().(*net.UDPAddr).Port, s.audioControl.LocalAddr().(*net.UDPAddr).Port, nil
	}
	data, err := net.ListenUDP("udp", &net.UDPAddr{IP: s.localIP, Zone: s.zone})
	if err != nil {
		return 0, 0, err
	}
	control, err := net.ListenUDP("udp", &net.UDPAddr{IP: s.localIP, Zone: s.zone})
	if err != nil {
		data.Close()
		return 0, 0, err
	}
	s.audioData, s.audioControl = data, control
	audio, err := newAudioStream(s, options, data, control)
	if err != nil {
		data.Close()
		control.Close()
		s.audioData, s.audioControl = nil, nil
		return 0, 0, err
	}
	s.audio = audio
	logging.Infof("airplay: audio ready ct=%d spf=%d format=0x%x; UDP data=%d control=%d", options.CT, options.SPF, options.Format, data.LocalAddr().(*net.UDPAddr).Port, control.LocalAddr().(*net.UDPAddr).Port)
	s.receiver.mu.Lock()
	s.receiver.audioOwner = s.owner
	s.receiver.mu.Unlock()
	s.wg.Add(1)
	go func() { defer s.wg.Done(); audio.run() }()
	return data.LocalAddr().(*net.UDPAddr).Port, control.LocalAddr().(*net.UDPAddr).Port, nil
}

func newMediaSession(r *Receiver, p *pairing, control net.Conn, key []byte, timingPort uint16) (*mediaSession, error) {
	local, peer := control.LocalAddr().(*net.TCPAddr), control.RemoteAddr().(*net.TCPAddr)
	timing, err := newTimingSession(local, peer, timingPort)
	if err != nil {
		return nil, err
	}
	return &mediaSession{receiver: r, owner: p, key: append([]byte(nil), key...), secret: append([]byte(nil), p.secret...), localIP: local.IP, peerIP: peer.IP, zone: local.Zone, timing: timing, stop: make(chan struct{})}, nil
}

func mirrorCipher(key, secret []byte, connectionID uint64) (cipher.Stream, error) {
	if len(key) != 16 || len(secret) != 32 {
		return nil, fmt.Errorf("airplay: invalid mirror key/secret")
	}
	h := sha512.New()
	h.Write(key)
	h.Write(secret)
	intermediate := h.Sum(nil)[:16]
	derive := func(label string) []byte {
		h := sha512.New()
		h.Write([]byte(label + strconv.FormatUint(connectionID, 10)))
		h.Write(intermediate)
		return h.Sum(nil)[:16]
	}
	block, err := aes.NewCipher(derive("AirPlayStreamKey"))
	if err != nil {
		return nil, err
	}
	return cipher.NewCTR(block, derive("AirPlayStreamIV")), nil
}

func (s *mediaSession) startMirror(connectionID uint64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		if connectionID != s.connectionID {
			return 0, fmt.Errorf("airplay: stream ID changed without teardown")
		}
		return s.listener.Addr().(*net.TCPAddr).Port, nil
	}
	stream, err := mirrorCipher(s.key, s.secret, connectionID)
	if err != nil {
		return 0, err
	}
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: s.localIP, Zone: s.zone})
	if err != nil {
		return 0, err
	}
	s.listener, s.connectionID = listener, connectionID
	s.mirrorStop, s.mirrorDone = make(chan struct{}), make(chan struct{})
	s.wg.Add(1)
	go s.receive(listener, NewMirrorDecoder(stream), s.mirrorStop, s.mirrorDone)
	return listener.Addr().(*net.TCPAddr).Port, nil
}

// Wait for the old worker before allowing a fresh cipher and stream ID. The
// per-stream stop also releases a worker blocked behind the frame queue.
func (s *mediaSession) stopMirror() {
	s.mu.Lock()
	listener, conn, done := s.listener, s.conn, s.mirrorDone
	if s.mirrorStop != nil {
		close(s.mirrorStop)
	}
	s.listener, s.conn, s.mirrorStop, s.mirrorDone = nil, nil, nil, nil
	s.mu.Unlock()
	if listener != nil {
		listener.Close()
	}
	if conn != nil {
		conn.Close()
	}
	if done != nil {
		<-done
	}
	// No pictures from the stopped stream should survive its next SETUP.
	s.receiver.mu.Lock()
	defer s.receiver.mu.Unlock()
	if s.receiver.owner == s.owner {
		for {
			select {
			case <-s.receiver.frames:
			default:
				return
			}
		}
	}
}

func (s *mediaSession) receive(listener net.Listener, decoder *MirrorDecoder, stopped, done chan struct{}) {
	defer s.wg.Done()
	defer close(done)
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		if !conn.RemoteAddr().(*net.TCPAddr).IP.Equal(s.peerIP) {
			conn.Close()
			continue
		}
		s.mu.Lock()
		select {
		case <-stopped:
			s.mu.Unlock()
			conn.Close()
			return
		case <-s.stop:
			s.mu.Unlock()
			conn.Close()
			return
		default:
		}
		s.conn = conn
		s.mu.Unlock()
		defer conn.Close()
		reader := bufio.NewReader(conn)
		reportSeen := false
		for {
			packet, err := readMirrorPacket(conn, reader, 30*time.Second)
			if err != nil {
				s.mediaFailed(stopped, err)
				return
			}
			arrived := time.Now()
			if packet.Type == 5 && !reportSeen {
				logging.Infof("airplay: received streaming report type=5 payload=%d; keeping mirror cipher continuous", len(packet.Payload))
				reportSeen = true
			}
			au, err := decoder.Decode(packet)
			if err != nil {
				s.mediaFailed(stopped, err)
				return
			}
			if packet.Type == 1 {
				logging.Infof("airplay: mirror AVC configuration %dx%d waiting_IDR=%t", decoder.width, decoder.height, decoder.waitIDR)
			}
			if len(au) == 0 {
				continue
			}
			frame := &capture.Frame{Data: au, Format: capture.FormatH264, Width: decoder.width, Height: decoder.height, CapturedAt: s.timing.presentationTime(packet.Timestamp, arrived)}
			// Bound memory without dropping dependent H.264 pictures. Closing
			// either the sender or receiver always unblocks this backpressure.
			s.receiver.mu.Lock()
			frames, stop := s.receiver.frames, s.receiver.stop
			s.receiver.mu.Unlock()
			select {
			case frames <- receivedFrame{frame: frame, owner: s.owner}:
			case <-stopped:
				return
			case <-s.stop:
				return
			case <-stop:
				return
			}
		}
	}
}

// A locked or static screen can leave the video stream idle indefinitely.
// Retry idle deadlines only before the first packet byte; a timed-out partial
// header or encrypted payload must remain fatal to avoid losing CTR alignment.
// Session teardown closes conn and unblocks the idle reader.
func readMirrorPacket(conn net.Conn, reader *bufio.Reader, timeout time.Duration) (*MirrorPacket, error) {
	for {
		if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			return nil, err
		}
		if _, err := reader.Peek(1); err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return nil, err
		}
		if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			return nil, err
		}
		return ReadMirrorPacket(reader)
	}
}

func (s *mediaSession) mediaFailed(stopped <-chan struct{}, err error) {
	select {
	case <-stopped:
		return
	case <-s.stop:
		return
	default:
	}
	logging.Infof("airplay: mirroring ended (%v); reconnect the sender to start a new session", err)
	// Cipher state cannot be reused after a truncated video payload.
	// Close the owning control connection so the next sender gets fresh keys.
	s.ownerControlClose()
}

func (s *mediaSession) ownerControlClose() {
	s.receiver.mu.Lock()
	control := s.owner.control
	s.receiver.mu.Unlock()
	if control != nil {
		control.Close()
	}
}

func (s *mediaSession) Close() {
	s.mu.Lock()
	select {
	case <-s.stop:
		s.mu.Unlock()
		return
	default:
		close(s.stop)
	}
	listener, conn, audioData, audioControl := s.listener, s.conn, s.audioData, s.audioControl
	s.mu.Unlock()
	if listener != nil {
		listener.Close()
	}
	if conn != nil {
		conn.Close()
	}
	if audioData != nil {
		audioData.Close()
		audioControl.Close()
	}
	s.timing.Close()
	s.wg.Wait()
	s.receiver.clearSessionAudio(s.owner)
}
