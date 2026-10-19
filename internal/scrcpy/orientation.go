package scrcpy

import (
	"fmt"
	"strings"

	"anykvm/internal/capture"
)

// ValidateCaptureOrientation accepts scrcpy 3.x capture orientation values.
// Empty leaves the server default; @ locks the initial orientation and a
// prefixed value such as @90 locks that orientation against device rotation.
func ValidateCaptureOrientation(value string) error {
	if value == "" || value == "@" {
		return nil
	}
	switch strings.TrimPrefix(value, "@") {
	case "0", "90", "180", "270", "flip0", "flip90", "flip180", "flip270":
		return nil
	default:
		return fmt.Errorf("invalid capture orientation %q (use 0, 90, 180, 270, optionally flip and/or @)", value)
	}
}

// SetGameStreamCaptureOptions applies the negotiated video bitrate, capped by
// the configured bitrate, and the GameStream orientation on the next Open.
// A nil profile restores the Web/VNC configuration. scrcpy-server maps input
// from rotated video coordinates back to the device.
func (c *Capture) SetGameStreamCaptureOptions(profile *capture.VideoStreamOptions) {
	c.sess.mu.Lock()
	defer c.sess.mu.Unlock()
	c.sess.captureOrientation = ""
	c.sess.captureBitRate = c.sess.opts.BitRate
	if profile != nil {
		c.sess.captureOrientation = c.sess.opts.GameStreamOrientation
		if profile.Bitrate > 0 {
			// GameStream negotiates kbps; scrcpy expects bits/sec.
			bitrate := profile.Bitrate * 1000
			if c.sess.opts.BitRate > 0 {
				bitrate = min(bitrate, c.sess.opts.BitRate)
			}
			c.sess.captureBitRate = bitrate
		}
	}
}

// serverArgs constructs the Android server invocation. Caller holds s.mu.
func (s *Session) serverArgs(version, scid string, maxSize, fps int) []string {
	args := []string{
		"shell",
		"CLASSPATH=" + remoteJarPath,
		"app_process", "/", "com.genymobile.scrcpy.Server", version,
		"scid=" + scid,
		"log_level=info",
		fmt.Sprintf("audio=%t", s.opts.Audio),
		"video_codec=" + s.videoCodec,
		"tunnel_forward=true",
		"send_device_meta=false",
	}
	if s.opts.Audio {
		args = append(args, "audio_codec=raw")
	}
	if s.opts.KeepActive {
		args = append(args, "keep_active=true")
	}
	if s.opts.Crop != "" {
		args = append(args, "crop="+s.opts.Crop)
	}
	if s.displayWidth > 0 && s.displayHeight > 0 {
		args = append(args, fmt.Sprintf("new_display=%dx%d", s.displayWidth, s.displayHeight))
	} else if maxSize > 0 {
		args = append(args, fmt.Sprintf("max_size=%d", maxSize))
	}
	if fps > 0 {
		args = append(args, fmt.Sprintf("max_fps=%d", fps))
	}
	if s.captureBitRate > 0 {
		args = append(args, fmt.Sprintf("video_bit_rate=%d", s.captureBitRate))
	}
	if s.opts.StayAwake {
		args = append(args, "stay_awake=true")
	}
	if s.captureOrientation != "" {
		args = append(args, "capture_orientation="+s.captureOrientation)
	}
	return args
}
