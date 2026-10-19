package videoenc

import (
	"anykvm/internal/av1"
	"encoding/binary"
	"fmt"
)

var mfStartCode = []byte{0, 0, 0, 1}

// Shared by native Windows and Raspberry Pi backends. Drivers
// can return Annex-B or length-prefixed NAL units.
// Recovery pictures include missing parameter sets from the negotiated type.
func mediaFoundationPacket(data, header []byte, codec string) ([]byte, error) {
	if codec == "av1" {
		_, err := av1.Inspect(data)
		return data, err
	}
	parameters, lengthSize, err := mediaFoundationHeader(header, codec)
	if err != nil {
		return nil, err
	}
	units, err := mediaFoundationUnits(data, lengthSize)
	if len(header) > 0 && header[0] == 1 {
		// A four-byte length of 256 begins with 00 00 01 00, which also
		// looks like an Annex-B prefix. The configuration record disambiguates it.
		if prefixed, prefixErr := mediaFoundationLengthUnits(data, lengthSize); prefixErr == nil {
			units, err = prefixed, nil
		}
	}
	if err != nil {
		return nil, err
	}
	present := make(map[byte]bool)
	recovery := false
	for _, unit := range units {
		tag := unit[0] & 31
		if unit[0]&0x80 != 0 {
			return nil, fmt.Errorf("invalid native video NAL header")
		}
		if codec == "hevc" {
			if len(unit) < 2 {
				return nil, fmt.Errorf("short native video HEVC NAL unit")
			}
			tag = (unit[0] >> 1) & 63
			if unit[1]&7 == 0 {
				return nil, fmt.Errorf("invalid native video HEVC temporal ID")
			}
			recovery = recovery || tag >= 16 && tag <= 23
		} else {
			if tag == 0 || tag > 23 {
				return nil, fmt.Errorf("invalid native video H.264 NAL type")
			}
			recovery = recovery || tag == 5
		}
		present[tag] = true
	}
	var output []byte
	if recovery {
		for _, parameter := range parameters {
			tag := parameter[0] & 31
			if codec == "hevc" {
				if len(parameter) < 2 {
					return nil, fmt.Errorf("short native video HEVC parameter set")
				}
				tag = (parameter[0] >> 1) & 63
				if tag < 32 || tag > 34 {
					continue
				}
			} else if tag != 7 && tag != 8 {
				continue
			}
			if !present[tag] {
				output = append(output, mfStartCode...)
				output = append(output, parameter...)
				present[tag] = true
			}
		}
		if codec == "h264" && (!present[7] || !present[8]) {
			return nil, fmt.Errorf("native video recovery picture lacks SPS/PPS")
		}
	}
	for _, unit := range units {
		output = append(output, mfStartCode...)
		output = append(output, unit...)
	}
	if len(output) == 0 || len(output) > 32*1024*1024 {
		return nil, fmt.Errorf("invalid native video access unit size")
	}
	return output, nil
}

func mediaFoundationUnits(data []byte, lengthSize int) ([][]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty native video NAL data")
	}
	startCode := func(at int) int {
		if at+3 <= len(data) && data[at] == 0 && data[at+1] == 0 {
			if data[at+2] == 1 {
				return 3
			}
			if at+4 <= len(data) && data[at+2] == 0 && data[at+3] == 1 {
				return 4
			}
		}
		return 0
	}
	var units [][]byte
	if startCode(0) != 0 {
		for offset := 0; offset < len(data); {
			begin := offset + startCode(offset)
			end := begin
			for end < len(data) && startCode(end) == 0 {
				end++
			}
			unitEnd := end
			for unitEnd > begin && data[unitEnd-1] == 0 {
				unitEnd--
			}
			if unitEnd == begin {
				return nil, fmt.Errorf("empty native video Annex-B NAL unit")
			}
			units = append(units, data[begin:unitEnd])
			offset = end
		}
		return units, nil
	}
	return mediaFoundationLengthUnits(data, lengthSize)
}

func mediaFoundationLengthUnits(data []byte, lengthSize int) ([][]byte, error) {
	if len(data) == 0 || lengthSize < 1 || lengthSize > 4 {
		return nil, fmt.Errorf("invalid native video NAL length size")
	}
	var units [][]byte
	for offset := 0; offset < len(data); {
		if len(data)-offset < lengthSize {
			return nil, fmt.Errorf("truncated native video NAL length")
		}
		length := uint32(0)
		for _, value := range data[offset : offset+lengthSize] {
			length = length<<8 | uint32(value)
		}
		offset += lengthSize
		if length == 0 || uint64(length) > uint64(len(data)-offset) {
			return nil, fmt.Errorf("invalid native video NAL length %d", length)
		}
		units = append(units, data[offset:offset+int(length)])
		offset += int(length)
	}
	return units, nil
}

func mediaFoundationHeader(header []byte, codec string) ([][]byte, int, error) {
	if len(header) == 0 {
		return nil, 4, nil
	}
	if header[0] != 1 {
		units, err := mediaFoundationUnits(header, 4)
		return units, 4, err
	}
	var units [][]byte
	read := func(offset *int) error {
		if len(header)-*offset < 2 {
			return fmt.Errorf("truncated native video parameter set length")
		}
		size := int(binary.BigEndian.Uint16(header[*offset:]))
		*offset += 2
		if size == 0 || size > len(header)-*offset {
			return fmt.Errorf("invalid native video parameter set length")
		}
		units = append(units, header[*offset:*offset+size])
		*offset += size
		return nil
	}
	if codec == "h264" {
		if len(header) < 7 {
			return nil, 0, fmt.Errorf("short native video AVC configuration")
		}
		offset := 6
		for i := 0; i < int(header[5]&31); i++ {
			if err := read(&offset); err != nil {
				return nil, 0, err
			}
		}
		if offset >= len(header) {
			return nil, 0, fmt.Errorf("missing native video PPS count")
		}
		count := int(header[offset])
		offset++
		for i := 0; i < count; i++ {
			if err := read(&offset); err != nil {
				return nil, 0, err
			}
		}
		return units, int(header[4]&3) + 1, nil
	}
	if len(header) < 23 {
		return nil, 0, fmt.Errorf("short native video HEVC configuration")
	}
	offset := 23
	for i := 0; i < int(header[22]); i++ {
		if len(header)-offset < 3 {
			return nil, 0, fmt.Errorf("truncated native video HEVC array")
		}
		count := int(binary.BigEndian.Uint16(header[offset+1:]))
		offset += 3
		for j := 0; j < count; j++ {
			if err := read(&offset); err != nil {
				return nil, 0, err
			}
		}
	}
	return units, int(header[21]&3) + 1, nil
}
