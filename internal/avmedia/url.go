package avmedia

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"strings"
)

// MediaPacket owns one compressed access unit from a multiplexed URL source.
// Timestamp and DecodeTimestamp are microseconds relative to the media origin.
// H.264 is Annex-B; AAC is raw with MPEG-4 AudioSpecificConfig.
type MediaPacket struct {
	Frame
	Audio                      bool
	Timestamp, DecodeTimestamp int64
	Config                     []byte
	SampleRate, Channels       int
}

func validateMediaURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 16384 || strings.ContainsAny(raw, "\r\n\x00") || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("media URL must use HTTP or HTTPS without user credentials")
	}
	return nil
}

func OpenMedia(ctx context.Context, raw string) (*MediaSource, error) {
	if err := validateMediaURL(raw); err != nil {
		return nil, err
	}
	return openMedia(ctx, raw)
}

func validMediaPosition(seconds float64) bool {
	return !math.IsNaN(seconds) && !math.IsInf(seconds, 0) && seconds >= 0 && seconds < float64(math.MaxInt64)/1e6
}
