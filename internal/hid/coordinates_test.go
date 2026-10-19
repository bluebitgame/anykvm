package hid

import (
	"math"
	"testing"
)

func TestNormalizedDisplayPointRoundTrip(t *testing.T) {
	for _, extent := range []int{720, 900, 1080, 1440, 1920, 3840} {
		for pixel := 0; pixel < extent; pixel++ {
			normalized := int(math.Round(float64(pixel) * 32767 / float64(extent-1)))
			if got := normalizedDisplayPoint(normalized, -1440, extent); got != -1440+pixel {
				t.Fatalf("extent=%d pixel=%d got=%d", extent, pixel, got)
			}
		}
	}
	if normalizedDisplayPoint(-1, 100, 900) != 100 || normalizedDisplayPoint(65535, 100, 900) != 999 {
		t.Fatal("normalized coordinates did not clamp to display bounds")
	}
}
