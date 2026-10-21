//go:build ffmpeg || !cgo || (!linux && !windows && !darwin)

package jpegcodec

import "fmt"

func LosslessJPEGTiles(data []byte, tileSize int) ([]JPEGTile, int, int, error) {
	return nil, 0, 0, fmt.Errorf("TurboJPEG lossless transform runtime unavailable")
}
