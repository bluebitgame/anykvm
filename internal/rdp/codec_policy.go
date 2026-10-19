package rdp

import (
	"errors"
	"strings"
)

// CodecOptions is the set of encodings a server permits during negotiation.
type CodecOptions struct {
	H264        bool `yaml:"h264"`
	Progressive bool `yaml:"progressive"`
	Planar      bool `yaml:"planar"`
	ClearCodec  bool `yaml:"clearcodec"`
	HEVC        bool `yaml:"hevc"` // Reserved until RDP HEVC wire support is implemented.
}

func DefaultCodecOptions() CodecOptions {
	return CodecOptions{H264: true, Progressive: true, Planar: true, ClearCodec: true}
}

func (o CodecOptions) Validate() error {
	if !o.H264 && !o.Progressive && !o.Planar && !o.ClearCodec {
		return errors.New("rdp.codecs must allow at least one implemented encoding")
	}
	return nil
}

func (o CodecOptions) String() string {
	var names []string
	for _, entry := range []struct {
		name    string
		enabled bool
	}{{"h264", o.H264}, {"progressive", o.Progressive}, {"planar", o.Planar}, {"clearcodec", o.ClearCodec}} {
		if entry.enabled {
			names = append(names, entry.name)
		}
	}
	return strings.Join(names, ",")
}
