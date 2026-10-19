//go:build viewer && darwin

package viewer

import (
	"os/exec"
	"strings"
	"sync"
)

var (
	macNaturalScrollOnce sync.Once
	macNaturalScrollOn   bool
)

// macNaturalScrolling reports Mac System Settings → Trackpad/Mouse → Natural scrolling.
func macNaturalScrolling() bool {
	macNaturalScrollOnce.Do(func() {
		out, err := exec.Command("defaults", "read", "-g", "com.apple.swipescrolldirection").Output()
		if err != nil {
			macNaturalScrollOn = true
			return
		}
		v := strings.TrimSpace(string(out))
		macNaturalScrollOn = v == "1" || strings.EqualFold(v, "true")
	})
	return macNaturalScrollOn
}

func platformNaturalWheelInvert() bool {
	return macNaturalScrolling()
}
