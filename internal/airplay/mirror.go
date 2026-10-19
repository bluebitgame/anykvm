package airplay

import (
	"bytes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"io"
)

const maxMirrorPayload = 16 << 20

// MirrorPacket is one native 128-byte-header AirPlay packet. Timestamp is the
// sender's NTP-format value, not local wall time. Do not manufacture a local
// capture timestamp until a timing session can map the sender's clock.
type MirrorPacket struct {
	Type      uint16
	Timestamp uint64
	Header    [128]byte
	Payload   []byte
}

func ReadMirrorPacket(r io.Reader) (*MirrorPacket, error) {
	p := new(MirrorPacket)
	if _, err := io.ReadFull(r, p.Header[:]); err != nil {
		return nil, err
	}
	size := binary.LittleEndian.Uint32(p.Header[:4])
	if size > maxMirrorPayload {
		return nil, fmt.Errorf("airplay: mirror payload exceeds limit")
	}
	p.Type = binary.LittleEndian.Uint16(p.Header[4:6]) & 0xff
	p.Timestamp = binary.LittleEndian.Uint64(p.Header[8:16])
	if p.Type > 2 && p.Type != 5 || p.Type == 2 && size != 0 {
		return nil, fmt.Errorf("airplay: unsupported mirror packet type=%d payload=%d", p.Type, size)
	}
	p.Payload = make([]byte, int(size))
	if _, err := io.ReadFull(r, p.Payload); err != nil {
		return nil, err
	}
	return p, nil
}

// MirrorDecoder converts avcC codec data and length-prefixed H.264 pictures to
// complete Annex-B access units. Its optional stream must already be initialized
// by an authenticated SETUP. CTR state continues across video payloads only;
// codec data, heartbeats and streaming reports never consume keystream bytes.
type MirrorDecoder struct {
	stream        cipher.Stream
	sets          []byte
	lengthSize    int
	waitIDR       bool
	width, height int
}

func NewMirrorDecoder(stream cipher.Stream) *MirrorDecoder {
	return &MirrorDecoder{stream: stream, waitIDR: true}
}

func (d *MirrorDecoder) Decode(p *MirrorPacket) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("airplay: missing mirror packet")
	}
	switch p.Type {
	case 2, 5:
		// Modern senders periodically send cleartext type-5 streaming reports.
		// They carry statistics, not H.264 or encrypted stream data.
		return nil, nil
	case 1:
		sets, size, err := parseAVCConfig(p.Payload)
		if err != nil {
			return nil, err
		}
		// The first SPS follows a 4-byte Annex-B prefix. Read its dimensions
		// instead of trusting unvalidated floating-point sizes in the header.
		end := 4
		for end+4 <= len(sets) && !(sets[end] == 0 && sets[end+1] == 0 && sets[end+2] == 0 && sets[end+3] == 1) {
			end++
		}
		info, err := parseH264SPS(sets[4:end])
		if err != nil {
			return nil, err
		}
		// A sender may repeat its configuration when returning from a quiet
		// or locked screen. Identical parameter sets do not invalidate decoded
		// references: keep accepting deltas without waiting for a new IDR.
		// A real configuration change still requires decoder recovery.
		if d.lengthSize != size || !bytes.Equal(d.sets, sets) {
			d.waitIDR = true
		}
		d.sets, d.lengthSize = sets, size
		d.width, d.height = info.Width, info.Height
		return nil, nil
	case 0:
		data := append([]byte(nil), p.Payload...)
		if d.stream != nil {
			d.stream.XORKeyStream(data, data)
		}
		if d.lengthSize == 0 {
			return nil, fmt.Errorf("airplay: video before AVC configuration")
		}
		var au []byte
		idr, vcl := false, false
		for off := 0; off < len(data); {
			if len(data)-off < d.lengthSize {
				return nil, fmt.Errorf("airplay: truncated NAL size")
			}
			var n uint32
			for _, b := range data[off : off+d.lengthSize] {
				n = n<<8 | uint32(b)
			}
			off += d.lengthSize
			if n == 0 || uint64(n) > uint64(len(data)-off) {
				return nil, fmt.Errorf("airplay: invalid NAL size")
			}
			nal := data[off : off+int(n)]
			t := nal[0] & 31
			if nal[0]&0x80 != 0 || t == 0 || t >= 24 {
				return nil, fmt.Errorf("airplay: invalid H.264 NAL")
			}
			idr, vcl = idr || t == 5, vcl || t >= 1 && t <= 5
			if len(au)+4+len(nal) > maxMirrorPayload {
				return nil, fmt.Errorf("airplay: access unit exceeds limit")
			}
			au = append(au, 0, 0, 0, 1)
			au = append(au, nal...)
			off += int(n)
		}
		if !vcl || d.waitIDR && !idr {
			return nil, nil
		}
		if idr {
			au = append(append([]byte(nil), d.sets...), au...)
			d.waitIDR = false
		}
		return au, nil
	default:
		return nil, fmt.Errorf("airplay: unsupported mirror packet")
	}
}

func parseAVCConfig(data []byte) ([]byte, int, error) {
	bad := func() ([]byte, int, error) { return nil, 0, fmt.Errorf("airplay: invalid AVC configuration") }
	if len(data) < 7 || len(data) > 64<<10 || data[0] != 1 || data[4]&3 == 2 {
		return bad()
	}
	size, off := int(data[4]&3)+1, 6
	var sets []byte
	for kind := 0; kind < 2; kind++ {
		count := int(data[5] & 31)
		if kind == 1 {
			if off >= len(data) {
				return bad()
			}
			count = int(data[off])
			off++
		}
		if count == 0 {
			return bad()
		}
		for i := 0; i < count; i++ {
			if len(data)-off < 2 {
				return bad()
			}
			n := int(binary.BigEndian.Uint16(data[off : off+2]))
			off += 2
			if n < 2 || n > len(data)-off || data[off]&0x80 != 0 || int(data[off]&31) != 7+kind {
				return bad()
			}
			sets = append(sets, 0, 0, 0, 1)
			sets = append(sets, data[off:off+n]...)
			off += n
		}
	}
	return sets, size, nil
}
