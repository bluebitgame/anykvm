package gamestream

import (
	"errors"
	"time"
)

// A sender owns this clock across capture and encoder restarts. Source monotonic
// microseconds and host diagnostic time never share an epoch. A constant RTP
// offset bridges placeholder/source changes without altering source intervals.
type videoPlaybackClock struct {
	initialized, sourceActive      bool
	sourceOrigin, lastPTS          int64
	sourceBase, last, legacyOffset uint32
}

func rtpMicroseconds(microseconds int64) uint32 {
	seconds, fraction := microseconds/1_000_000, microseconds%1_000_000
	return uint32(seconds*90000 + (fraction*90000+500000)/1_000_000)
}

func frameRTPStep(fps int) uint32 {
	if fps <= 0 {
		fps = 60
	}
	return uint32(max(1, (90000+fps/2)/fps))
}

func (c *videoPlaybackClock) timestamp(pts int64, capturedAt, epoch, now time.Time, fps int, repeat bool) (uint32, error) {
	if pts < 0 || pts > 1<<53-1 {
		return 0, errors.New("invalid source playback PTS")
	}
	var stamp uint32
	if pts > 0 {
		if repeat && c.initialized && c.sourceActive && pts == c.lastPTS {
			// Re-encoding retained raw pixels creates a new output picture.
			// Advance the rate-control timeline without inventing a Go PTS.
			c.last += frameRTPStep(fps)
			return c.last, nil
		}
		if !c.sourceActive || pts < c.lastPTS {
			c.sourceOrigin, c.sourceBase = pts, 90 // nonzero initial RTP time (1 ms)
			if c.initialized {
				c.sourceBase = c.last + frameRTPStep(fps)
			}
		}
		stamp = c.sourceBase + rtpMicroseconds(pts-c.sourceOrigin)
		if c.sourceActive && pts > c.lastPTS && int32(stamp-c.last) <= 0 {
			// A fresh source picture can catch up with a retained-picture
			// rate-control clock. Rebase once, then preserve later source deltas.
			c.sourceOrigin, c.sourceBase = pts, c.last+frameRTPStep(fps)
			stamp = c.sourceBase
		}
		c.sourceActive, c.lastPTS = true, pts
	} else {
		stamp = captureRTPTimestamp(capturedAt, epoch, now)
		if c.sourceActive {
			c.legacyOffset = c.last + frameRTPStep(fps) - stamp
			c.sourceActive = false
		}
		stamp += c.legacyOffset
	}
	c.initialized, c.last = true, stamp
	return stamp, nil
}
