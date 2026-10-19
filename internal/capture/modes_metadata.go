package capture

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"sync"
)

// Kept platform-independent so device metadata can be checked without hardware.
func decodeDeviceVideoModes(data []byte) ([]VideoMode, error) {
	var modes []VideoMode
	if err := json.Unmarshal(data, &modes); err != nil {
		return nil, fmt.Errorf("invalid capture mode metadata: %w", err)
	}
	result := make([]VideoMode, 0, len(modes))
	seen := map[VideoMode]bool{}
	for _, mode := range modes {
		if mode.Width <= 0 || mode.Height <= 0 || mode.Width > 65535 || mode.Height > 65535 || mode.MinFPS <= 0 || mode.MaxFPS < mode.MinFPS || mode.MaxFPS > 1000 || math.IsNaN(mode.MinFPS) || math.IsNaN(mode.MaxFPS) {
			continue
		}
		if _, err := ParseMode(fmt.Sprintf("%dx%d:%s", mode.Width, mode.Height, mode.PixelFormat)); err != nil || mode.PixelFormat == "" {
			continue
		}
		if !seen[mode] {
			seen[mode] = true
			result = append(result, mode)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.Width != b.Width {
			return a.Width > b.Width
		}
		if a.Height != b.Height {
			return a.Height > b.Height
		}
		if a.PixelFormat != b.PixelFormat {
			return a.PixelFormat < b.PixelFormat
		}
		if a.MaxFPS != b.MaxFPS {
			return a.MaxFPS > b.MaxFPS
		}
		return a.MinFPS < b.MinFPS
	})
	if len(result) == 0 {
		return nil, fmt.Errorf("device reported no usable capture modes")
	}
	return result, nil
}

// Keep capabilities available while a driver holds its input exclusively.
// Inactive inputs are queried again, so unplug/replug does not leave stale modes.
type deviceModeCache struct {
	mu      sync.Mutex
	entries map[string]*deviceModeCacheEntry
}
type deviceModeCacheEntry struct {
	modes []VideoMode
	users int
}

func (c *deviceModeCache) list(device string, discover func() ([]VideoMode, error)) ([]VideoMode, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]*deviceModeCacheEntry{}
	}
	entry := c.entries[device]
	if entry != nil && entry.users > 0 && len(entry.modes) > 0 {
		return append([]VideoMode(nil), entry.modes...), nil
	}
	modes, err := discover()
	if err != nil {
		return nil, err
	}
	if entry == nil {
		entry = &deviceModeCacheEntry{}
		c.entries[device] = entry
	}
	entry.modes = append([]VideoMode(nil), modes...)
	return append([]VideoMode(nil), entry.modes...), nil
}
func (c *deviceModeCache) use(device string, delta int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.entries[device]; entry != nil {
		entry.users = max(0, entry.users+delta)
	}
}
