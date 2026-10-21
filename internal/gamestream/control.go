package gamestream

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"time"

	"anykvm/internal/gamestream/native"
	"anykvm/internal/logging"
)

func controlAEAD(key []byte, v2 bool) (cipher.AEAD, error) {
	b, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	n := 16
	if v2 {
		n = 12
	}
	return cipher.NewGCMWithNonceSize(b, n)
}
func controlNonce(seq uint32, v2 bool, side byte) []byte {
	if !v2 {
		iv := make([]byte, 16)
		iv[0] = byte(seq)
		return iv
	}
	iv := make([]byte, 12)
	binary.LittleEndian.PutUint32(iv, seq)
	iv[10] = side
	iv[11] = 'C'
	return iv
}
func openControl(a cipher.AEAD, v2 bool, data []byte) (uint16, uint32, []byte, error) {
	if len(data) < 28 || binary.LittleEndian.Uint16(data) != 1 || int(binary.LittleEndian.Uint16(data[2:])) != len(data)-4 {
		return 0, 0, nil, errors.New("invalid control envelope")
	}
	seq := binary.LittleEndian.Uint32(data[4:])
	sealed := append(append([]byte{}, data[24:]...), data[8:24]...)
	plain, err := a.Open(nil, controlNonce(seq, v2, 'C'), sealed, nil)
	if err != nil {
		return 0, 0, nil, err
	}
	if len(plain) < 4 || int(binary.LittleEndian.Uint16(plain[2:])) != len(plain)-4 {
		return 0, 0, nil, errors.New("invalid control message length")
	}
	return binary.LittleEndian.Uint16(plain), seq, plain[4:], nil
}
func sealControl(a cipher.AEAD, v2 bool, seq uint32, kind uint16, data []byte) []byte {
	plain := make([]byte, 4+len(data))
	binary.LittleEndian.PutUint16(plain, kind)
	binary.LittleEndian.PutUint16(plain[2:], uint16(len(data)))
	copy(plain[4:], data)
	sealed := a.Seal(nil, controlNonce(seq, v2, 'H'), plain, nil)
	out := make([]byte, 24+len(plain))
	binary.LittleEndian.PutUint16(out, 1)
	binary.LittleEndian.PutUint16(out[2:], uint16(len(out)-4))
	binary.LittleEndian.PutUint32(out[4:], seq)
	copy(out[8:24], sealed[len(plain):])
	copy(out[24:], sealed[:len(plain)])
	return out
}

// Different ENet channels may reorder encrypted sequence numbers. A window
// accepts reordering but rejects duplicate/replayed authenticated messages.
type replayWindow struct {
	highest uint32
	seen    map[uint32]bool
}

func (w *replayWindow) accept(seq uint32) bool {
	if w.seen == nil {
		w.seen = make(map[uint32]bool)
	}
	if w.seen[seq] || (w.highest > seq && w.highest-seq >= 4096) {
		return false
	}
	if seq > w.highest {
		w.highest = seq
		for n := range w.seen {
			if seq > n && seq-n >= 4096 {
				delete(w.seen, n)
			}
		}
	}
	w.seen[seq] = true
	return true
}

func (s *Server) controlLoop() {
	restorePriority := prioritizeInputThread()
	defer restorePriority()
	defer s.control.Close()
	var current *streamSession
	var peer uint32
	var a cipher.AEAD
	var v2 bool
	var replay replayWindow
	var inputs *inputQueue
	var statsAt time.Time
	for {
		if current != nil && current.ctx.Err() != nil {
			// Allow ENet to flush the encrypted termination before resetting peer.
			_ = s.control.Send(peer, sealControl(a, v2, 0, 0x0109, []byte{0x80, 0x03, 0x00, 0x23}))
			s.control.Reset(peer)
			current = nil
		}
		if s.ctx.Err() != nil {
			return
		}
		e, err := s.control.Poll(5)
		if err != nil {
			if x := s.currentSession(); x != nil {
				x.stop("ENet error")
			}
			logging.Infof("gamestream: %v", err)
			return
		}
		switch e.Type {
		case native.Connect:
			x := s.currentSession()
			if x == nil || x.ctx.Err() != nil || !sameIP(e.Address, x.ip) || current != nil {
				s.control.Reset(e.Peer)
				continue
			}
			x.mu.Lock()
			ready := x.announced
			opts := x.options
			x.mu.Unlock()
			if !ready || (opts.Features&2 != 0 && e.ConnectData != x.connectData) {
				s.control.Reset(e.Peer)
				continue
			}
			v2 = opts.Encryption&1 != 0
			a, err = controlAEAD(x.key, v2)
			if err != nil {
				s.control.Reset(e.Peer)
				continue
			}
			current, peer, replay = x, e.Peer, replayWindow{}
			inputs = newInputQueue(x.ctx, &x.input)
			statsAt = time.Now()
			x.lastControl.Store(time.Now().UnixNano())
			logging.Infof("gamestream: ENet control connected from %s", e.Address)
		case native.Disconnect:
			if current != nil && e.Peer == peer {
				current.stop("GameStream client disconnected")
				current = nil
			}
		case native.Receive:
			if current == nil || e.Peer != peer || current.ctx.Err() != nil {
				continue
			}
			kind, seq, payload, err := openControl(a, v2, e.Data)
			if err != nil {
				current.stop("control authentication failed")
				continue
			}
			if !replay.accept(seq) {
				continue
			}
			now := time.Now()
			current.lastControl.Store(now.UnixNano())
			if now.Sub(statsAt) >= 5*time.Second {
				if stats, ok := s.control.Stats(peer); ok {
					logging.Debugf("gamestream: control network rtt=%dms rtt_variation=%dms reliable_loss=%.2f%%", stats.RTTMillis, stats.RTTVariationMillis, stats.ReliableLossPercent)
				}
				statsAt = now
			}
			switch kind {
			case 0x0206:
				if err := inputs.enqueue(payload); err != nil {
					logging.Infof("gamestream: input enqueue: %v", err)
					current.stop("input queue rejected packet")
				}
			case 0x0302, 0x0301:
				if kind == 0x0302 {
					logging.Infof("gamestream: client requested IDR (control=0x0302)")
				} else {
					logging.Infof("gamestream: client requested reference invalidation (control=0x0301); responding with IDR")
				}
				select {
				case current.keyframe <- struct{}{}:
				default:
				}
			case 0x0109:
				current.stop("client termination")
				// Start, ping, loss reports, FEC status and optional device events
				// don't require responses. Unsupported gamepad/text events are ignored.
			}
		}
	}
}
