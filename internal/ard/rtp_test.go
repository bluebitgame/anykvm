package ard

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

func rtpPacket(seq uint16, stamp uint32, marker bool, nal []byte) []byte {
	p := make([]byte, 12)
	p[0], p[1] = 128, 100
	if marker {
		p[1] |= 128
	}
	put16(p[2:], int(seq))
	put32(p[4:], stamp)
	put32(p[8:], 0x11223344)
	return append(p, nal...)
}
func TestRTPReorderingLossAndWrap(t *testing.T) {
	r := rtpReorder{}
	now := time.Now()
	for _, seq := range []uint16{65534, 0, 65535} {
		packets, gap := r.push(rtpPacket(seq, 1, false, []byte{2, 1}), now)
		if gap {
			t.Fatal("false gap on wrap")
		}
		if seq == 0 && len(packets) != 0 {
			t.Fatal("released out-of-order packet")
		}
		if seq == 65535 && (len(packets) != 2 || be16(packets[0][2:]) != 65535 || be16(packets[1][2:]) != 0) {
			t.Fatal("incorrect wrap order")
		}
	}
	r.push(rtpPacket(2, 2, false, []byte{2, 1}), now)
	if out, gap := r.flush(now.Add(31 * time.Millisecond)); !gap || len(out) != 1 {
		t.Fatal("lost packet not bounded")
	}
	if out, _ := r.push(rtpPacket(1, 2, false, []byte{2, 1}), now); len(out) != 0 {
		t.Fatal("accepted late packet")
	}
}
func installSets(a *hevcAssembler) {
	for typ := byte(32); typ <= 34; typ++ {
		a.nal([]byte{typ << 1, 1, 42})
	}
}
func TestHEVCFragmentsMultiSliceAndRecovery(t *testing.T) {
	a := hevcAssembler{waiting: true}
	installSets(&a)
	// A two-fragment IDR followed by a second slice in the same access unit.
	p1 := rtpPacket(1, 100, false, []byte{98, 1, 128 | 19, 11, 12})
	p2 := rtpPacket(2, 100, false, []byte{98, 1, 64 | 19, 13, 14})
	for _, p := range [][]byte{p1, p2} {
		au, _, _, err := a.feed(p)
		if err != nil || len(au) != 0 {
			t.Fatal("published incomplete picture", err)
		}
	}
	au, stamp, key, err := a.feed(rtpPacket(3, 100, true, []byte{38, 1, 15, 16}))
	if err != nil || !key || stamp != 100 || !bytes.Contains(au, []byte{0, 0, 0, 1, 38, 1, 11, 12, 13, 14}) || !bytes.Contains(au, []byte{0, 0, 0, 1, 38, 1, 15, 16}) {
		t.Fatalf("multi-slice AU=%x key=%v err=%v", au, key, err)
	}
	a.loss()
	au, _, _, _ = a.feed(rtpPacket(4, 101, true, []byte{2, 1, 9}))
	if len(au) != 0 {
		t.Fatal("published references after loss")
	}
	au, _, _, _ = a.feed(rtpPacket(5, 102, true, []byte{38, 1, 9}))
	if len(au) == 0 {
		t.Fatal("failed IRAP recovery")
	}
	a.feed(p1)
	a.loss()
	au, _, _, _ = a.feed(rtpPacket(6, 100, true, []byte{98, 1, 64 | 19, 13}))
	if len(au) != 0 {
		t.Fatal("published lost fragment")
	}
}
func TestHEVCAggregationAndMalformedLengths(t *testing.T) {
	a := hevcAssembler{waiting: true}
	p := []byte{96, 1}
	for typ := byte(32); typ <= 34; typ++ {
		p = binary.BigEndian.AppendUint16(p, 3)
		p = append(p, typ<<1, 1, 9)
	}
	if _, _, _, err := a.feed(rtpPacket(1, 1, false, p)); err != nil {
		t.Fatal(err)
	}
	au, _, _, err := a.feed(rtpPacket(2, 1, true, []byte{38, 1, 9}))
	if err != nil || len(au) == 0 {
		t.Fatal("aggregation did not install parameters", err)
	}
	if _, _, _, err := a.feed(rtpPacket(3, 2, false, []byte{96, 1, 255, 255})); err == nil {
		t.Fatal("accepted oversized aggregation")
	}
	for _, p := range [][]byte{{}, make([]byte, 12), {144, 100, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 255, 255}} {
		if _, err := rtpHeaderLen(p); err == nil {
			t.Fatal("accepted bad RTP header")
		}
	}
}
func FuzzRTPAndSPS(f *testing.F) {
	f.Add(rtpPacket(1, 1, true, []byte{38, 1, 9}))
	f.Add([]byte{96, 1, 255, 255})
	f.Fuzz(func(t *testing.T, p []byte) {
		if len(p) > 65535 {
			return
		}
		parseSPS(p)
		if _, err := rtpHeaderLen(p); err == nil {
			a := hevcAssembler{waiting: true}
			a.feed(p)
		}
	})
}
