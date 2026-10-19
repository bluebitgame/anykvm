package capture

import "fmt"

// HEVC may retain two inputs before producing output. Keep its existing safe
// minimum even when the target selects a smaller H.264 in-flight budget.
func screenEncoderFrameLimit(codec string, configured int) (int, error) {
	if configured < 0 || configured > 8 {
		return 0, fmt.Errorf("buffer_frames must be 0..8")
	}
	if configured == 0 {
		configured = 2
	}
	if codec == "hevc" {
		configured = max(4, configured)
	}
	return configured, nil
}
