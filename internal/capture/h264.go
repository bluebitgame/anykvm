package capture

const defaultH264BitrateKbps = 12000

// AUParser incrementally splits an Annex-B byte stream into access units,
// using AUD boundaries when audDelimited is set, as required for multi-slice
// FFmpeg H.264 output. Legacy mode groups each VCL slice with preceding NALs.
type AUParser struct {
	audDelimited bool
	data         []byte
	synced       bool
	curAU        []byte
}

// push appends chunk and returns any access units that became complete.
func (p *AUParser) Push(chunk []byte) [][]byte {
	p.data = append(p.data, chunk...)

	if !p.synced {
		i := findStartCode(p.data, 0)
		if i < 0 {
			if len(p.data) > 3 {
				p.data = p.data[len(p.data)-3:]
			}
			return nil
		}
		p.data = p.data[i:]
		p.synced = true
	}

	var aus [][]byte
	for {
		j := findStartCode(p.data, 3)
		if j < 0 {
			return aus
		}
		nal := p.data[:j]
		p.data = append([]byte(nil), p.data[j:]...)
		if au := p.addNAL(nal); au != nil {
			aus = append(aus, au)
		}
	}
}

// addNAL appends a NAL to the current access unit and returns the completed
// access unit when the NAL is a VCL slice.
func (p *AUParser) addNAL(nal []byte) []byte {
	if p.audDelimited {
		if nalType(nal) == 9 && len(p.curAU) > 0 {
			au := p.curAU
			p.curAU = append([]byte(nil), nal...)
			return au
		}
		p.curAU = append(p.curAU, nal...)
		return nil
	}
	p.curAU = append(p.curAU, nal...)
	t := nalType(nal)
	if t >= 1 && t <= 5 {
		au := p.curAU
		p.curAU = nil
		return au
	}
	return nil
}

// FlushPending returns the buffered-but-not-yet-delimited access unit, used when
// the encoder has flushed a whole frame and the pipe went idle (so the trailing
// NAL is known complete without waiting for the next frame's start code). It
// only emits when the pending NAL is a VCL slice, i.e. a full coded picture is
// buffered; otherwise it returns nil and keeps waiting.
func (p *AUParser) FlushPending() []byte {
	if p.audDelimited {
		return nil
	} // Idle does not prove a multi-slice frame is complete.
	if !p.synced || len(p.data) == 0 {
		return nil
	}
	if t := nalType(p.data); t < 1 || t > 5 {
		return nil
	}
	au := append(p.curAU, p.data...)
	p.curAU = nil
	p.data = p.data[:0]
	return au
}

// findStartCode returns the index of the next 0x000001 start code at or after
// from, or -1 if none is present.
func findStartCode(b []byte, from int) int {
	for i := from; i+2 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			return i
		}
	}
	return -1
}

// nalType returns the H.264 NAL unit type of a NAL that begins with a start
// code (3- or 4-byte), or 0 if it cannot be determined.
func nalType(nal []byte) byte {
	for i := 0; i+3 < len(nal); i++ {
		if nal[i] == 0 && nal[i+1] == 0 && nal[i+2] == 1 {
			return nal[i+3] & 0x1F
		}
	}
	return 0
}
