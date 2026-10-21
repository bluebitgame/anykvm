package capture

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Mode describes an input capture mode, independently of the output encoder.
type Mode struct {
	Width, Height int
	FPS           float64
	PixelFormat   string
	FollowClient  bool
	Scale         float64 // physical screen pixels; defaults to 1; used when Width and Height are zero
}

var modePattern = regexp.MustCompile(`^(?i:(auto)|([1-9][0-9]*)x([1-9][0-9]*))?(?:@([0-9]+(?:\.[0-9]+)?))?(?::([a-zA-Z0-9_]+))?$`)
var fourCCFormatPattern = regexp.MustCompile(`^(fourcc_[0-9a-f]{8}|guid_[0-9a-f]{32})$`)
var scalePattern = regexp.MustCompile(`^(?:[0-9]+)?\.[0-9]+$`)

// ParseMode accepts WxH[@FPS][:format], SCALE[@FPS], auto[@FPS], @FPS, or empty.
func ParseMode(s string) (Mode, error) {
	s = strings.TrimSpace(s)
	mode := Mode{Scale: 1}
	value := s
	end := strings.IndexAny(value, "@:")
	if end < 0 {
		end = len(value)
	}
	if scalePattern.MatchString(value[:end]) {
		mode.Scale, _ = strconv.ParseFloat(value[:end], 64)
		if mode.Scale <= 0 || math.IsInf(mode.Scale, 0) {
			return Mode{}, fmt.Errorf("invalid capture scale in %q; use a positive decimal", s)
		}
		value = value[end:]
	}
	m := modePattern.FindStringSubmatch(value)
	if m == nil {
		return Mode{}, fmt.Errorf("invalid capture mode %q; use WIDTHxHEIGHT@FPS:format or 0.75@30", s)
	}
	mode.FollowClient = m[1] != ""
	if m[2] != "" {
		mode.Width, _ = strconv.Atoi(m[2])
		mode.Height, _ = strconv.Atoi(m[3])
		if mode.Width <= 0 || mode.Height <= 0 || mode.Width > 65535 || mode.Height > 65535 {
			return Mode{}, fmt.Errorf("invalid capture dimensions in %q", s)
		}
	}
	if m[4] != "" {
		mode.FPS, _ = strconv.ParseFloat(m[4], 64)
		if mode.FPS <= 0 || mode.FPS > 1000 || math.IsInf(mode.FPS, 0) {
			return Mode{}, fmt.Errorf("invalid capture frame rate in %q", s)
		}
	}
	mode.PixelFormat = strings.ToLower(m[5])
	switch mode.PixelFormat {
	case "", "nv12", "nv12_full", "yuyv422", "uyvy422", "mjpeg", "jpeg", "bgra", "bgr0", "bgr24", "i420", "iyuv", "yv12", "p010", "p016", "y210", "y216", "v210", "v216", "h264":
	default:
		if !fourCCFormatPattern.MatchString(mode.PixelFormat) {
			return Mode{}, fmt.Errorf("unknown capture pixel format %q", mode.PixelFormat)
		}
	}
	if mode.FollowClient && mode.PixelFormat != "" {
		return Mode{}, fmt.Errorf("auto follows the client and cannot specify an input pixel format")
	}
	if scalePattern.MatchString(s[:end]) && mode.PixelFormat != "" {
		return Mode{}, fmt.Errorf("screen scaling cannot specify an input pixel format")
	}
	return mode, nil
}

// VideoMode is one device-reported format and its supported frame-rate range.
type VideoMode struct {
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	PixelFormat string  `json:"pixelFormat"`
	MinFPS      float64 `json:"minFPS"`
	MaxFPS      float64 `json:"maxFPS"`
}

func ConfigureMode(device Device, value string) error {
	mode, err := ParseMode(value)
	if err != nil {
		return err
	}
	if scaler, ok := device.(interface{ SetOutputScale(float64) error }); ok {
		if err := scaler.SetOutputScale(mode.Scale); err != nil {
			return err
		}
	} else if mode.Scale != 1 {
		return fmt.Errorf("proportional screen scaling is unavailable for this source")
	}
	if setter, ok := device.(interface{ SetCaptureMode(string) error }); ok {
		return setter.SetCaptureMode(value)
	}
	if mode.PixelFormat != "" {
		return fmt.Errorf("capture format selection is unavailable for this source on this platform")
	}
	return nil
}
