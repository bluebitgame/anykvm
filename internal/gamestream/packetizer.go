package gamestream

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"sync"

	"anykvm/internal/gamestream/native"
)

const videoFECPercentage = 20

type videoPacketizer struct {
	playback videoPlaybackClock
	pacer    videoPacer
	sequence uint32
	frame    uint32
	iv       uint64
	gcm      cipher.AEAD
}

// Packet batches own their final FEC/encryption storage until UDP sends finish.
// Size classes avoid retaining a large keyframe buffer for tiny later frames.
var videoBuffers [13]sync.Pool // 512 bytes through 2 MiB; reclaimed by Go's GC

type packetBatch struct {
	packets [][]byte
	blocks  [4]*[]byte // up to four combined FEC/encryption blocks
	count   int
}

func videoBufferClass(size int) (int, int) {
	class, capacity := 0, 512
	for capacity < size && class < len(videoBuffers)-1 {
		class++
		capacity *= 2
	}
	return class, capacity
}
func (b *packetBatch) alloc(size int, pooled bool) []byte {
	if !pooled {
		return make([]byte, size)
	}
	class, capacity := videoBufferClass(size)
	var storage *[]byte
	if value := videoBuffers[class].Get(); value != nil {
		storage = value.(*[]byte)
	} else {
		data := make([]byte, capacity)
		storage = &data
	}
	b.blocks[b.count] = storage
	b.count++
	data := (*storage)[:size]
	clear(data) // FEC covers padding and reserved header bytes too.
	return data
}
func (b *packetBatch) Release() {
	if b == nil {
		return
	}
	for i := 0; i < b.count; i++ {
		storage := b.blocks[i]
		class, _ := videoBufferClass(cap(*storage))
		videoBuffers[class].Put(storage)
		b.blocks[i] = nil
	}
	b.count = 0
	b.packets = nil
}

// pack returns owned packet buffers that do not need to be returned to a pool.
func (v *videoPacketizer) pack(au []byte, keyframe bool, timestamp uint32, size, minFEC int) ([][]byte, error) {
	batch, err := v.packInto(au, keyframe, timestamp, size, minFEC, false)
	if err != nil {
		return nil, err
	}
	return batch.packets, nil
}
func (v *videoPacketizer) packBatch(au []byte, keyframe bool, timestamp uint32, size, minFEC int) (*packetBatch, error) {
	return v.packInto(au, keyframe, timestamp, size, minFEC, true)
}

func (v *videoPacketizer) packInto(au []byte, keyframe bool, timestamp uint32, size, minFEC int, pooled bool) (_ *packetBatch, err error) {
	batch := &packetBatch{}
	defer func() {
		if err != nil {
			batch.Release()
		}
	}()
	if len(au) == 0 || size < 512 || size > 1408 || minFEC < 0 || minFEC > 255 {
		return nil, errors.New("invalid video frame/packet size")
	}
	// The negotiated packet size excludes the 16-byte RTP header, but includes
	// the 16-byte NV header. A short frame header precedes the encoded payload (Annex B or AV1 OBUs).
	payloadSize := size - 16
	if len(au) > 4*212*payloadSize-8 {
		return nil, errors.New("encoded frame exceeds four FEC blocks; lower resolution/bitrate")
	}
	blockSize := size + 16
	var header [8]byte
	data := header[:]
	dataSize := len(au) + len(header)
	data[0] = 1
	data[3] = 1
	if keyframe {
		data[3] = 2
	}
	last := (dataSize-1)%payloadSize + 1
	binary.LittleEndian.PutUint16(data[4:], uint16(last))
	count := (dataSize + payloadSize - 1) / payloadSize
	blocks := (count + 211) / 212
	if blocks > 4 {
		return nil, errors.New("encoded frame exceeds four FEC blocks; lower resolution/bitrate")
	}
	perBlock := (count + blocks - 1) / blocks
	v.frame++
	batch.packets = make([][]byte, 0, count+(count*videoFECPercentage+99)/100+blocks*max(0, minFEC))
	for b := 0; b < blocks; b++ {
		ds := min(perBlock, count-b*perBlock)
		ps := (ds*videoFECPercentage + 99) / 100
		pct := videoFECPercentage
		if ps < minFEC {
			ps = minFEC
			pct = 100 * ps / ds
		}
		if ds+ps > 255 || pct > 255 {
			return nil, errors.New("invalid video FEC layout")
		}
		stride, offset := blockSize, 0
		if v.gcm != nil {
			stride, offset = blockSize+32+16, 32
		}
		shards := batch.alloc((ds+ps)*stride, pooled)
		for i := 0; i < ds; i++ {
			start := i*stride + offset
			p := shards[start : start+blockSize]
			binary.LittleEndian.PutUint32(p[16:], (v.sequence+uint32(i))<<8)
			binary.LittleEndian.PutUint32(p[20:], v.frame)
			p[24] = 1
			if i == 0 {
				p[24] |= 4
			}
			if i == ds-1 {
				p[24] |= 2
			}
			p[26] = 0x10
			p[27] = byte(b<<4 | (blocks-1)<<6)
			start = (b*perBlock + i) * payloadSize
			end := min(start+payloadSize, dataSize)
			dst := p[32:]
			if start < len(header) {
				n := copy(dst, header[start:min(end, len(header))])
				dst, start = dst[n:], start+n
			}
			if start < end {
				copy(dst, au[start-len(header):end-len(header)])
			}
		}
		if err := native.CodeStrided(shards, ds, ps, blockSize, stride, offset, false, nil); err != nil {
			return nil, err
		}
		for i := 0; i < ds+ps; i++ {
			start := i*stride + offset
			p := shards[start : start+blockSize]
			p[0] = 0x90
			binary.BigEndian.PutUint16(p[2:], uint16(v.sequence+uint32(i)))
			binary.BigEndian.PutUint32(p[4:], timestamp)
			binary.LittleEndian.PutUint32(p[20:], v.frame)
			p[27] = byte(b<<4 | (blocks-1)<<6)
			binary.LittleEndian.PutUint32(p[28:], uint32(i<<12|ds<<22|pct<<4))
			if v.gcm != nil {
				out := shards[i*stride : (i+1)*stride : (i+1)*stride]
				iv := out[:12]
				binary.LittleEndian.PutUint64(iv, v.iv)
				iv[11] = 'V'
				v.iv++
				binary.LittleEndian.PutUint32(out[12:], v.frame)
				// Encrypt the FEC shard in place. The GCM tag lands in the reserved
				// tail and is moved to the protocol's tag field; no second AU buffer.
				sealed := v.gcm.Seal(out[32:32], iv, p, nil)
				copy(out[16:32], sealed[len(p):])
				out = out[:32+len(p)]
				p = out
			}
			batch.packets = append(batch.packets, p)
		}
		v.sequence += uint32(ds + ps)
	}
	return batch, nil
}

type audioPacketizer struct {
	sequence         uint16
	timestamp, keyID uint32
	block            cipher.Block
	duration         int
	pending          [][]byte
}

func (a *audioPacketizer) pack(opus []byte) ([][]byte, error) {
	payload := append([]byte(nil), opus...)
	if a.block != nil {
		pad := aes.BlockSize - len(payload)%aes.BlockSize
		for j := 0; j < pad; j++ {
			payload = append(payload, byte(pad))
		}
		iv := make([]byte, 16)
		binary.BigEndian.PutUint32(iv, a.keyID+uint32(a.sequence))
		cipher.NewCBCEncrypter(a.block, iv).CryptBlocks(payload, payload)
	}
	p := make([]byte, 12+len(payload))
	p[0] = 0x80
	p[1] = 97
	binary.BigEndian.PutUint16(p[2:], a.sequence)
	binary.BigEndian.PutUint32(p[4:], a.timestamp)
	copy(p[12:], payload)
	result := [][]byte{p}
	a.pending = append(a.pending, payload)
	if len(a.pending) == 4 {
		size := len(payload)
		equal := true
		for _, v := range a.pending {
			if len(v) != size {
				equal = false
			}
		}
		if equal { // GameStream audio FEC requires fixed-size (CBR) Opus packets.
			shards := make([]byte, 6*size)
			for i, v := range a.pending {
				copy(shards[i*size:], v)
			}
			if err := native.Code(shards, 4, 2, size, true, nil); err != nil {
				return nil, err
			}
			for i := 0; i < 2; i++ {
				fec := make([]byte, 24+size)
				fec[0] = 0x80
				fec[1] = 127
				binary.BigEndian.PutUint16(fec[2:], a.sequence+uint16(i)+1)
				fec[12] = byte(i)
				fec[13] = 97
				binary.BigEndian.PutUint16(fec[14:], a.sequence-3)
				binary.BigEndian.PutUint32(fec[16:], a.timestamp-uint32(3*a.duration))
				copy(fec[24:], shards[(4+i)*size:(5+i)*size])
				result = append(result, fec)
			}
		}
		a.pending = nil
	}
	a.sequence++
	a.timestamp += uint32(a.duration)
	return result, nil
}
