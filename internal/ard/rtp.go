package ard

import (
	"fmt"
	"time"
)

func rtpHeaderLen(p []byte) (int, error) {
	if len(p) < 12 || p[0]>>6 != 2 {
		return 0, fmt.Errorf("ard: invalid RTP header")
	}
	n := 12 + int(p[0]&15)*4
	if n > len(p) {
		return 0, fmt.Errorf("ard: truncated RTP CSRC")
	}
	if p[0]&16 != 0 {
		if n+4 > len(p) {
			return 0, fmt.Errorf("ard: truncated RTP extension")
		}
		n += 4 + int(be16(p[n+2:]))*4
	}
	if n > len(p) {
		return 0, fmt.Errorf("ard: invalid RTP extension length")
	}
	return n, nil
}

type rtpReorder struct {
	started bool
	next    uint16
	pending map[uint16][]byte
	since   time.Time
}

// A maximum of 64 datagrams / 30ms may wait for a missing sequence. A lost
// packet invalidates the whole access unit; never feed a partial picture.
func (r *rtpReorder) push(p []byte, now time.Time) (out [][]byte, gap bool) {
	seq := be16(p[2:])
	if !r.started {
		r.started = true
		r.next = seq
		r.pending = make(map[uint16][]byte)
	}
	d := uint16(seq - r.next)
	if d >= 32768 {
		return nil, false
	}
	if d >= 64 {
		r.pending = make(map[uint16][]byte)
		r.next = seq
		gap = true
	}
	r.pending[seq] = p
	if len(r.pending) > 0 && r.since.IsZero() {
		r.since = now
	}
	if now.Sub(r.since) >= 30*time.Millisecond {
		gap = r.skipMissing() || gap
	}
	for {
		p, ok := r.pending[r.next]
		if !ok {
			break
		}
		delete(r.pending, r.next)
		r.next++
		out = append(out, p)
	}
	if len(r.pending) == 0 {
		r.since = time.Time{}
	}
	return
}
func (r *rtpReorder) skipMissing() bool {
	if len(r.pending) == 0 {
		return false
	}
	min := uint16(65535)
	for seq := range r.pending {
		d := uint16(seq - r.next)
		if d < min {
			min = d
		}
	}
	if min == 0 {
		return false
	}
	r.next += min
	return true
}
func (r *rtpReorder) flush(now time.Time) (out [][]byte, gap bool) {
	if r.since.IsZero() || now.Sub(r.since) < 30*time.Millisecond {
		return
	}
	gap = r.skipMissing()
	for {
		p, ok := r.pending[r.next]
		if !ok {
			break
		}
		delete(r.pending, r.next)
		r.next++
		out = append(out, p)
	}
	if len(r.pending) == 0 {
		r.since = time.Time{}
	} else {
		r.since = now
	}
	return
}

const maxAccessUnit = 16 << 20

type hevcAssembler struct {
	stamp                 uint32
	active                bool
	unit, fu              []byte
	sets                  [3][]byte
	hasVCL, irap, waiting bool
}

func (a *hevcAssembler) loss() {
	a.unit = nil
	a.fu = nil
	a.active = false
	a.hasVCL = false
	a.irap = false
	a.waiting = true
}
func (a *hevcAssembler) nal(n []byte) error {
	if len(n) < 2 || n[0]&128 != 0 || n[1]&7 == 0 {
		return fmt.Errorf("ard: invalid HEVC NAL header")
	}
	t := (n[0] >> 1) & 63
	if t >= 32 && t <= 34 {
		a.sets[t-32] = append([]byte(nil), n...)
		return nil
	}
	if t < 32 {
		a.hasVCL = true
		if t >= 16 && t <= 23 {
			a.irap = true
		}
	}
	if len(a.unit)+len(n)+4 > maxAccessUnit {
		return fmt.Errorf("ard: HEVC access unit exceeds limit")
	}
	a.unit = append(a.unit, 0, 0, 0, 1)
	a.unit = append(a.unit, n...)
	return nil
}
func (a *hevcAssembler) feed(p []byte) (au []byte, stamp uint32, recovery bool, err error) {
	h, e := rtpHeaderLen(p)
	if e != nil {
		err = e
		return
	}
	end := len(p)
	if p[0]&32 != 0 {
		n := int(p[end-1])
		if n == 0 || n > end-h {
			err = fmt.Errorf("ard: invalid RTP padding")
			return
		}
		end -= n
	}
	n := p[h:end]
	if len(n) < 2 {
		err = fmt.Errorf("ard: truncated HEVC payload")
		return
	}
	ts := be32(p[4:])
	if a.active && ts != a.stamp {
		if a.hasVCL || len(a.fu) > 0 {
			a.loss()
		}
		a.unit = nil
	}
	a.stamp = ts
	a.active = true
	switch (n[0] >> 1) & 63 {
	case 48:
		for off := 2; off < len(n); {
			if off+2 > len(n) {
				err = fmt.Errorf("ard: truncated HEVC aggregation")
				return
			}
			size := int(be16(n[off:]))
			off += 2
			if size < 2 || size > len(n)-off {
				err = fmt.Errorf("ard: invalid HEVC aggregation size")
				return
			}
			if err = a.nal(n[off : off+size]); err != nil {
				return
			}
			off += size
		}
	case 49:
		if len(n) < 4 {
			err = fmt.Errorf("ard: truncated HEVC fragment")
			return
		}
		if n[2]&128 != 0 {
			if len(a.fu) != 0 {
				a.loss()
				a.stamp = ts
				a.active = true
			}
			a.fu = []byte{n[0]&129 | (n[2]&63)<<1, n[1]}
		}
		if len(a.fu) == 0 {
			a.loss()
			return
		}
		if len(a.fu)+len(n)-3 > maxAccessUnit {
			err = fmt.Errorf("ard: HEVC fragment exceeds limit")
			return
		}
		a.fu = append(a.fu, n[3:]...)
		if n[2]&64 != 0 {
			err = a.nal(a.fu)
			a.fu = nil
			if err != nil {
				return
			}
		}
	default:
		if len(a.fu) > 0 {
			a.loss()
			return
		}
		if err = a.nal(n); err != nil {
			return
		}
	}
	if p[1]&128 == 0 {
		return
	}
	if len(a.fu) > 0 {
		a.loss()
		return
	}
	if !a.hasVCL {
		a.unit = nil
		a.active = false
		return
	}
	if a.waiting && !a.irap {
		a.loss()
		return
	}
	if a.irap {
		for _, set := range a.sets {
			if len(set) == 0 {
				a.loss()
				return
			}
		}
		for _, set := range a.sets {
			au = append(au, 0, 0, 0, 1)
			au = append(au, set...)
		}
	}
	au = append(au, a.unit...)
	stamp = a.stamp
	recovery = a.irap
	a.unit = nil
	a.active = false
	a.hasVCL = false
	a.irap = false
	a.waiting = false
	return
}
