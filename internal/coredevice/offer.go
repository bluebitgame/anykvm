package coredevice

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"strings"

	"github.com/google/uuid"
	"howett.net/plist"
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

// DisplayService expects a binary plist around a compressed AVConference
// protobuf. Keep the padded session ID and tier order of Apple's video offer.
// Protocol reference: go-ios ios/display/offer.go, commit c5840429 (MIT).
func videoOffer(callID uuid.UUID, ssrc uint32) ([]byte, error) {
	var settings protobuf
	settings = binary.AppendUvarint(settings, 1<<3)
	value := uint64(ssrc)
	for i := 0; i < 5; i++ {
		b := byte(value & 127)
		value >>= 7
		if i < 4 {
			b |= 128
		}
		settings = append(settings, b)
	}
	settings.number(2, 0)
	for _, pt := range []uint64{123, 100} {
		var bank protobuf
		bank.number(1, pt)
		count := 2
		if pt == 123 {
			count = 4
		}
		for i := 0; i < count; i++ {
			var entry protobuf
			entry.number(1, 1)
			entry.number(2, uint64(1+i%2))
			entry.number(3, 50115)
			entry.number(4, 0)
			bank.data(2, entry)
		}
		// VRAE:0 disables resolution adaptation. Leave it out to avoid frame
		// drops under motion. A bank's PT number does not identify the NAL codec.
		bank.str(3, "FLS;SW:1;")
		trailer := uint64(14)
		if pt == 123 {
			trailer = 1
		}
		bank.number(4, trailer)
		settings.data(3, bank)
	}
	settings.number(7, 0) // Disable long-term references; recover via keyframes.
	settings.number(8, 63)
	// FEC is not advertised: this receiver requests fresh keyframes after loss.
	settings.number(12, 1)
	var blob protobuf
	blob.number(1, 1)
	blob.number(2, 1)
	blob.data(5, settings)
	return packMediaOffer(blob, callID, 5)
}

// AVConference audio uses negotiation mode 6 and audio settings in field 3.
// AAC-ELD is selected by the DisplayService AudioStreamMode response.
func audioOffer(callID uuid.UUID, ssrc uint32) ([]byte, error) {
	var settings, blob protobuf
	settings.number(1, uint64(ssrc))
	settings.number(2, 0)
	settings.number(3, 0)
	settings.number(4, 24191)
	settings.number(5, 0)
	settings.number(6, 0)
	blob.number(1, 1)
	blob.number(2, 1)
	blob.data(3, settings)
	return packMediaOffer(blob, callID, 6)
}

func packMediaOffer(blob protobuf, callID uuid.UUID, mode int) ([]byte, error) {
	blob.str(6, "Viceroy 1.7.0")
	blob.number(8, 0)
	for _, v := range [][3]uint64{{4074, 0, 16384}, {0, 75000000, 524288}, {0, 40000000, 12288}, {16, 4100, 0}, {0, 20000000, 98304}, {4, 6500, 0}, {0, 6000000, 131072}, {0, 100000000, 1048576}, {0, 60000000, 262144}, {1, 299, 0}} {
		var tier protobuf
		tier.number(1, v[0])
		tier.number(2, v[1])
		if v[2] != 0 {
			tier.number(3, v[2])
		}
		blob.data(9, tier)
	}
	blob.number(13, 17137042128614416384)
	blob.number(14, 2)
	blob.number(16, 0)
	blob.number(18, 1)
	var endpoint protobuf
	endpoint.number(1, 0)
	endpoint.number(2, 1)
	endpoint.str(3, "Mac15,9")
	endpoint.str(4, "2205.3.1")
	endpoint.str(5, "25F80")
	var compressed bytes.Buffer
	w, _ := zlib.NewWriterLevel(&compressed, zlib.BestCompression)
	if _, err := w.Write(blob); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return plist.Marshal(map[string]interface{}{
		"avcMediaStreamNegotiatorMediaBlob": compressed.Bytes(), "avcMediaStreamNegotiatorMode": mode,
		"avcMediaStreamOptionCallID": strings.ToUpper(callID.String()), "avcMediaStreamOptionRemoteEndpointInfo": []byte(endpoint),
	}, plist.BinaryFormat)
}
