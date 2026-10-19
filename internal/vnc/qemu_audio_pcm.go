package vnc

import (
	"encoding/binary"

	"anykvm/internal/capture"
)

// Sample format values defined by the QEMU Audio extension.
const (
	qemuAudioU8 = iota
	qemuAudioS8
	qemuAudioU16
	qemuAudioS16
	qemuAudioU32
	qemuAudioS32

	qemuAudioMaxSampleRate = 48000
	captureMaxSampleRate   = 192000
	pcmBytesPerSample      = 2 // capture PCM is always S16LE
)

type qemuAudioFormat struct {
	sampleFormat, channels byte
	sampleRate             uint32
}

func (f qemuAudioFormat) valid() bool {
	// Match QEMU's upper bound, and reject zero before resampling arithmetic.
	return f.sampleFormat <= qemuAudioS32 &&
		(f.channels == 1 || f.channels == 2) &&
		f.sampleRate > 0 && f.sampleRate <= qemuAudioMaxSampleRate
}

// qemuAudioConverter converts interleaved S16LE capture to the requested PCM
// format. Linear interpolation carries its phase and previous sample across
// packets, so packet boundaries do not introduce rate drift or discontinuities.
type qemuAudioConverter struct {
	sourceRate, sourceChannels int
	sourceEpoch                uint64
	inputIndex                 int64
	nextOutputPosition         int64 // position in units of 1/outputRate input samples
	previous                   [2]int64
}

func (s *qemuAudioConverter) convert(a capture.AudioFrame, f qemuAudioFormat) []byte {
	rate, channels := a.SampleRate, a.Channels
	if rate == 0 {
		rate = capture.AudioSampleRate
	}
	if channels == 0 {
		channels = capture.AudioChannels
	}
	if !f.valid() || rate < 1 || rate > captureMaxSampleRate || (channels != 1 && channels != 2) || len(a.PCM)%(channels*pcmBytesPerSample) != 0 {
		return nil
	}
	if s.sourceRate != rate || s.sourceChannels != channels || s.sourceEpoch != a.Epoch {
		*s = qemuAudioConverter{sourceRate: rate, sourceChannels: channels, sourceEpoch: a.Epoch}
	}
	// Preserve the common capture format without copying or resampling.
	if f.sampleFormat == qemuAudioS16 && int(f.channels) == channels && int(f.sampleRate) == rate {
		return a.PCM
	}
	var out []byte
	for i := 0; i < len(a.PCM); i += channels * pcmBytesPerSample {
		left := int64(int16(binary.LittleEndian.Uint16(a.PCM[i:])))
		right := left
		if channels == 2 {
			right = int64(int16(binary.LittleEndian.Uint16(a.PCM[i+2:])))
		}
		current := [2]int64{left, right}
		if s.inputIndex == 0 {
			s.previous = current
		}
		boundary := s.inputIndex * int64(f.sampleRate)
		for s.nextOutputPosition <= boundary {
			sample := s.interpolate(current, int64(f.sampleRate))
			out = f.appendFrame(out, sample)
			s.nextOutputPosition += int64(rate)
		}
		s.previous = current
		s.inputIndex++
	}
	return out
}

// interpolate locates the next output sample between the previous and current
// input frames. Positions use output-rate ticks to avoid floating-point drift.
func (s *qemuAudioConverter) interpolate(current [2]int64, outputRate int64) [2]int64 {
	if s.inputIndex == 0 {
		return current
	}
	fraction := s.nextOutputPosition - (s.inputIndex-1)*outputRate
	var sample [2]int64
	for channel := range sample {
		sample[channel] = s.previous[channel] + (current[channel]-s.previous[channel])*fraction/outputRate
	}
	return sample
}

func (f qemuAudioFormat) appendFrame(out []byte, sample [2]int64) []byte {
	if f.channels == 1 {
		mono := (sample[0] + sample[1]) / 2
		return appendQEMUSample(out, mono, f.sampleFormat)
	}
	out = appendQEMUSample(out, sample[0], f.sampleFormat)
	return appendQEMUSample(out, sample[1], f.sampleFormat)
}

func appendQEMUSample(out []byte, sample int64, format byte) []byte {
	switch format {
	case qemuAudioU8:
		return append(out, byte((sample+32768)>>8))
	case qemuAudioS8:
		return append(out, byte(sample>>8))
	case qemuAudioU16:
		return binary.LittleEndian.AppendUint16(out, uint16(sample+32768))
	case qemuAudioS16:
		return binary.LittleEndian.AppendUint16(out, uint16(sample))
	case qemuAudioU32:
		return binary.LittleEndian.AppendUint32(out, uint32((sample+32768)<<16))
	default: // S32 (validated before conversion)
		return binary.LittleEndian.AppendUint32(out, uint32(sample<<16))
	}
}
