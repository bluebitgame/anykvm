//go:build viewer

package viewer

// ScrollMode controls how SDL wheel events are mapped before HID forwarding.
type ScrollMode string

const (
	ScrollNatural     ScrollMode = "natural"     // darwin only: invert when Mac natural scrolling is on
	ScrollTraditional ScrollMode = "traditional" // darwin only: never auto-invert
	ScrollInvert      ScrollMode = "invert"      // all platforms: always invert
	ScrollNone        ScrollMode = "none"        // pass through unchanged
)

func wheelInvertForMode(mode ScrollMode) bool {
	if mode == ScrollInvert {
		return true
	}
	if mode == ScrollNatural {
		return platformNaturalWheelInvert()
	}
	return false
}

func wheelDeltaFromSDL(sdlY int32, invert bool) int {
	if sdlY == 0 {
		return 0
	}
	delta := int(sdlY)
	if delta > 127 {
		delta = 127
	} else if delta < -128 {
		delta = -128
	}
	if invert {
		delta = -delta
	}
	return delta
}
