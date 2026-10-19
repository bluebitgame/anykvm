package ard

import (
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"time"
)

type protobuf []byte

func (p *protobuf) number(field, value uint64) {
	*p = binary.AppendUvarint(*p, field<<3)
	*p = binary.AppendUvarint(*p, value)
}
func (p *protobuf) data(field uint64, value []byte) {
	*p = binary.AppendUvarint(*p, field<<3|2)
	*p = binary.AppendUvarint(*p, uint64(len(value)))
	*p = append(*p, value...)
}
func (p *protobuf) str(field uint64, value string) { p.data(field, []byte(value)) }

// A small write-only binary-plist encoder for the four fields in a media offer.
// All objects are ASCII strings, data, small integers, or this single dictionary.
func mediaPlist(blob, endpoint []byte, mode byte, callID string) []byte {
	keys := []string{"avcMediaStreamNegotiatorMediaBlob", "avcMediaStreamNegotiatorMode", "avcMediaStreamOptionCallID", "avcMediaStreamOptionRemoteEndpointInfo"}
	objects := make([][]byte, 0, 9)
	object := func(kind byte, p []byte) []byte {
		if len(p) < 15 {
			return append([]byte{kind | byte(len(p))}, p...)
		}
		h := []byte{kind | 15, 0x12}
		h = binary.BigEndian.AppendUint32(h, uint32(len(p)))
		return append(h, p...)
	}
	for _, k := range keys {
		objects = append(objects, object(0x50, []byte(k)))
	}
	objects = append(objects, object(0x40, blob), []byte{0x10, mode}, object(0x50, []byte(callID)), object(0x40, endpoint))
	objects = append(objects, []byte{0xd4, 0, 1, 2, 3, 4, 5, 6, 7})
	out := []byte("bplist00")
	offsets := make([]uint32, len(objects))
	for i, o := range objects {
		offsets[i] = uint32(len(out))
		out = append(out, o...)
	}
	table := len(out)
	for _, off := range offsets {
		out = binary.BigEndian.AppendUint32(out, off)
	}
	trailer := make([]byte, 32)
	trailer[6], trailer[7] = 4, 1
	binary.BigEndian.PutUint64(trailer[8:], uint64(len(objects)))
	binary.BigEndian.PutUint64(trailer[16:], 8)
	binary.BigEndian.PutUint64(trailer[24:], uint64(table))
	return append(out, trailer...)
}

func randomUUID() (string, []byte, error) {
	p := make([]byte, 16)
	if _, err := rand.Read(p); err != nil {
		return "", nil, err
	}
	p[6] = p[6]&15 | 64
	p[8] = p[8]&63 | 128
	return fmt.Sprintf("%X-%X-%X-%X-%X", p[:4], p[4:6], p[6:8], p[8:10], p[10:]), p, nil
}
func mediaOffer(audio bool, ssrc uint32) ([]byte, error) {
	var settings protobuf
	settings.number(1, uint64(ssrc))
	settings.number(2, 0)
	mode := byte(7)
	if audio {
		mode = 8
		settings.number(3, 0)
		settings.number(4, 24191)
		settings.number(5, 0)
		settings.number(6, 0)
	} else {
		for _, pt := range []uint64{123, 100} {
			var payload protobuf
			payload.number(1, pt)
			count := 1
			if pt == 123 {
				count = 2
			}
			for i := 0; i < count; i++ {
				for _, op := range []uint64{1, 2} {
					var rule protobuf
					rule.number(1, 1)
					rule.number(2, op)
					rule.number(3, 50115)
					rule.number(4, 0)
					payload.data(2, rule)
				}
			}
			if pt == 123 {
				payload.str(3, "FLS;MS:-1;LF:-1;LTR;CABAC;POS:0;EOD:1;HTS:2;RR:3;AR:16/9,5/8;XR:16/9,5/8;")
				payload.number(4, 1)
			} else {
				payload.str(3, "FLS;LF:-1;POS:5;EOD:1;HTS:2;RR:3;POSE:4;AR:16/9,5/8;XR:16/9,5/8;")
				payload.number(4, 14)
			}
			settings.data(3, payload)
		}
		settings.number(6, 1) // One complete picture; compound subframes are unsupported.
		settings.number(7, 1)
		settings.number(8, 63)
		settings.number(9, 1)
		settings.number(12, 1)
	}
	var blob protobuf
	blob.number(1, 1)
	blob.number(2, 1)
	if audio {
		blob.data(3, settings)
	} else {
		blob.data(5, settings)
	}
	blob.str(6, "Viceroy 1.7.0")
	blob.number(8, 0)
	for _, v := range [][3]uint64{{4074, 0, 16384}, {4, 6500, 0}, {0, 40000000, 12288}, {0, 60000000, 262144}, {0, 20000000, 98304}, {0, 100000000, 1048576}, {0, 6000000, 131072}, {0, 75000000, 524288}, {16, 4100, 0}, {1, 299, 0}} {
		var b protobuf
		b.number(1, v[0])
		b.number(2, v[1])
		if v[2] != 0 {
			b.number(3, v[2])
		}
		blob.data(9, b)
	}
	now := time.Now()
	ntp := uint64(now.Unix()+2208988800)<<32 | uint64(now.Nanosecond())*(1<<32)/1000000000
	blob.number(13, ntp)
	blob.number(14, 2)
	blob.number(16, 0)
	blob.number(18, 1)
	var compressed bytes.Buffer
	z := zlib.NewWriter(&compressed)
	if _, err := z.Write(blob); err != nil {
		return nil, err
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	var endpoint protobuf
	endpoint.number(1, 0)
	endpoint.number(2, 1)
	endpoint.str(3, "AnyKVM")
	endpoint.str(4, "1")
	endpoint.str(5, "AnyKVM")
	id, _, err := randomUUID()
	if err != nil {
		return nil, err
	}
	return mediaPlist(compressed.Bytes(), endpoint, mode, id), nil
}

func viewerInfo() []byte {
	p := make([]byte, 66)
	p[0] = 0x21
	put16(p[2:], 62)
	// The native viewer's version/capability words, followed by reserved space.
	copy(p[4:], []byte{0, 1, 0, 0, 0, 2, 0, 0, 0, 6, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 26, 0, 0, 0, 3, 0, 0, 0, 1, 176, 0, 12, 3, 144, 0, 0, 0, 0, 64})
	return p
}
func mediaRequest() []byte { return []byte{0x12, 0, 0, 1, 0, 1, 0, 1, 0, 0, 0, 1, 10, 0, 0, 1} }
func encodingMessage(enc []int32) []byte {
	p := make([]byte, 4)
	p[0] = 2
	put16(p[2:], len(enc))
	for _, e := range enc {
		p = binary.BigEndian.AppendUint32(p, uint32(e))
	}
	return p
}

func (s *session) configureMedia() error {
	if s.configured {
		return nil
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	audioSSRC := be32(random[:])
	localSSRC := be32(random[4:])
	if localSSRC == 0 {
		localSSRC = 1
	}
	audio, err := mediaOffer(true, audioSSRC)
	if err != nil {
		return err
	}
	video, err := mediaOffer(false, localSSRC)
	if err != nil {
		return err
	}
	p := make([]byte, 220+len(audio)+len(video))
	p[0] = 0x1c
	put16(p[2:], len(p)-4)
	put16(p[4:], 3)
	flags := uint32(4)
	if s.fps >= 60 {
		flags |= 1
	}
	put32(p[6:], flags)
	put16(p[10:], len(audio))
	put16(p[12:], len(video))
	_, id, err := randomUUID()
	if err != nil {
		return err
	}
	copy(p[20:], id)
	keys := make([]byte, 184)
	if _, err := rand.Read(keys); err != nil {
		return err
	}
	copy(p[36:128], keys[:92])
	copy(p[128:], audio)
	off := 128 + len(audio)
	copy(p[off:], keys[92:])
	copy(p[off+92:], video)
	recv, err := newMediaCrypto(keys[138:], 0)
	if err != nil {
		return err
	}
	send, err := newMediaCrypto(keys[92:138], 3)
	if err != nil {
		return err
	}
	s.mediaMu.Lock()
	s.receiver = &srtpReceiver{mediaCrypto: recv}
	s.feedback = &srtcpSender{mediaCrypto: send}
	s.localSSRC = localSSRC
	s.generation++
drain:
	for {
		select {
		case <-s.frames:
		default:
			break drain
		}
	}
	s.mediaMu.Unlock()
	s.configured = true
	return s.write(p)
}
