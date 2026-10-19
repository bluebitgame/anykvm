package audiodec

import (
	"anykvm/internal/capture"
	"encoding/binary"
	"fmt"
)

// ALAC uses a bare 24-byte ALACSpecificConfig on AudioToolbox. FFmpeg expects
// the same config wrapped in a 36-byte 'alac' full box instead of an AAC ASC.
func alacMagicCookie(f capture.AudioFrame) ([]byte, error) {
	config := f.Config
	if len(config) != 24 || config[4] != 0 || config[5] != 16 || int(config[9]) != f.Channels ||
		int(binary.BigEndian.Uint32(config[:4])) != f.FramesPerPacket ||
		int(binary.BigEndian.Uint32(config[20:])) != f.SampleRate ||
		f.SampleRate < 8000 || f.SampleRate > 192000 || f.Channels < 1 || f.Channels > 2 ||
		f.FramesPerPacket < 1 || f.FramesPerPacket > 4096 {
		return nil, fmt.Errorf("audio: invalid ALAC configuration")
	}
	return append([]byte(nil), config...), nil
}

// MPEG-4 Systems ES_Descriptor containing DecoderConfig and DecoderSpecificInfo.
// The compressed access unit is unaffected by this host decoder configuration.
func aacMagicCookie(asc []byte) ([]byte, error) {
	if len(asc) < 2 || len(asc) > 64 {
		return nil, fmt.Errorf("audio: invalid AAC configuration size")
	}
	config := []byte{0x40, 0x15, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0} // MPEG-4 Audio, stream type 5
	config = append(config, 5, byte(len(asc)))
	config = append(config, asc...)
	es := []byte{0, 0, 0, 4, byte(len(config))}
	es = append(es, config...)
	es = append(es, 6, 1, 2)
	return append([]byte{3, byte(len(es))}, es...), nil
}
