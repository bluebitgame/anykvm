//go:build (!darwin && !windows) || (darwin && !cgo) || (windows && ffmpeg)

package gamestream

import "errors"

func newNativeOpusEncoder(duration int) (nativeOpusEncoder, error) {
	return nil, errors.New("system Opus encoder unavailable on this platform")
}
