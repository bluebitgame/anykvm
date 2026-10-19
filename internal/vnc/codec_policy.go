package vnc

// CodecOptions controls optional encodings. Raw, Hextile, Zlib and ZRLE
// are always available, subject to the client's advertised capabilities.
type CodecOptions struct {
	JPEG  bool `yaml:"jpeg"`
	Tight bool `yaml:"tight"`
	H264  bool `yaml:"h264"`
}

func DefaultCodecOptions() CodecOptions {
	return CodecOptions{JPEG: true, Tight: true, H264: true}
}

func (p *CodecOptions) allows(enc int32) bool {
	if p == nil {
		return true
	}
	switch enc {
	case encJPEG:
		return p.JPEG
	case encTight:
		return p.Tight
	case encH264:
		return p.H264
	default:
		return true
	}
}
