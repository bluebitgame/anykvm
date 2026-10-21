package spice

import (
	"encoding/binary"
	"fmt"

	"anykvm/internal/capture"
)

const (
	playbackMinRate       = 8000
	playbackMaxRate       = 192000
	playbackSampleRate    = 48000
	playbackMaxVolume     = 65535
	microsecondsPerSecond = 1000000
	// At most 200 ms of S16 stereo at the highest supported input rate.
	maxAudioPacketBytes = playbackMaxRate * 2 * 2 / 5
)

// Integer-phase linear interpolation preserves phase across packet boundaries.
// Discontinuities (restart, format change or queue drop) reset interpolation so
// unrelated samples are never blended. PCM and small cursor images use CPU RAM;
// the video DMA-BUF path remains unchanged.
type pcmConverter struct {
	rate, channels  int
	epoch, expected uint64

	// Positions use integer ticks: an input frame advances playbackSampleRate
	// ticks, while an output frame advances rate ticks.
	inputIndex         int64
	nextOutputPosition int64
	outputFrames       int64
	previous           [2]int64
	baseTimestamp      int64
}

func (c *pcmConverter) convert(p audioPacket) (*capture.AudioFrame, error) {
	if p.rate < playbackMinRate || p.rate > playbackMaxRate ||
		(p.channels != 1 && p.channels != 2) || len(p.pcm) == 0 ||
		len(p.pcm)%(2*p.channels) != 0 || len(p.pcm) > p.rate*p.channels*2/5 {
		return nil, fmt.Errorf("unsupported SPICE playback PCM format or packet size")
	}
	if c.rate != p.rate || c.channels != p.channels || c.epoch != p.epoch || c.expected != p.sample {
		*c = pcmConverter{
			rate: p.rate, channels: p.channels, epoch: p.epoch,
			baseTimestamp: int64(p.sample) * microsecondsPerSecond / int64(p.rate),
		}
	}
	timestamp := c.baseTimestamp + c.outputFrames*microsecondsPerSecond/playbackSampleRate
	samples := len(p.pcm) / (p.channels * 2)
	out := make([]byte, 0, (samples*playbackSampleRate/p.rate+2)*4)
	for i := 0; i < len(p.pcm); i += p.channels * 2 {
		left := int64(int16(binary.LittleEndian.Uint16(p.pcm[i:])))
		right := left
		if p.channels == 2 {
			right = int64(int16(binary.LittleEndian.Uint16(p.pcm[i+2:])))
		}
		current := [2]int64{left, right}
		if c.inputIndex == 0 {
			c.previous = current
		}
		boundary := c.inputIndex * playbackSampleRate
		for c.nextOutputPosition <= boundary {
			for ch := 0; ch < 2; ch++ {
				sample := current[ch]
				if c.inputIndex > 0 {
					fraction := c.nextOutputPosition - (c.inputIndex-1)*playbackSampleRate
					sample = c.previous[ch] + (current[ch]-c.previous[ch])*fraction/playbackSampleRate
				}
				// Apply current volume after interpolation, so a mute cannot leak a
				// cached unmuted sample from the previous packet.
				if p.mute {
					sample = 0
				} else {
					sample = sample * int64(p.volume[ch]) / playbackMaxVolume
				}
				out = binary.LittleEndian.AppendUint16(out, uint16(int16(sample)))
			}
			c.nextOutputPosition += int64(p.rate)
			c.outputFrames++
		}
		c.previous = current
		c.inputIndex++
	}
	c.expected = p.sample + uint64(samples)
	if len(out) == 0 {
		return nil, nil
	}
	return &capture.AudioFrame{PCM: out, SampleRate: playbackSampleRate, Channels: 2, Timestamp: timestamp}, nil
}
