package capture

import "math"

// DesktopSize describes requested display pixels. On Retina, the selected
// logical desktop may be smaller while its backing pixels match this request.
type DesktopSize struct {
	Width, Height int
	RefreshRate   int // Hz; zero preserves the current refresh rate
	ScalePercent  int // UI size in output pixels per logical point; zero preserves the current scale
}

// WithScaleProtection prevents UI enlargement below 1080 output rows.
func (size DesktopSize) WithScaleProtection() DesktopSize {
	if size.Height > 0 && size.Height < 1080 {
		size.ScalePercent = 100
	}
	return size
}

// DesktopConfigurer is implemented only by local displays that can change
// their desktop mode. Capture Close must not restore it: codec/keyframe
// restarts belong to the same controlling session.
type DesktopConfigurer interface {
	SetDesktopSize(DesktopSize) (DesktopSize, error)
	RestoreDesktopSize() error
	DesktopDisplayID() uint32
}

type displayMode struct {
	width, height, pixelWidth, pixelHeight int
	refreshRate                            float64
	index                                  int
}

// nearestDisplayMode compares height first when the client is wider than a
// candidate and width first when it is narrower. Equal aspect ratios compare
// long edges first. That backing-pixel edge must reach the requested size;
// the other edge may be smaller. A client DPI request ranks eligible modes
// by logical desktop dimensions to approximate the requested UI size.
func nearestDisplayMode(modes []displayMode, size DesktopSize, current displayMode) (displayMode, bool) {
	if size.Width <= 0 || size.Height <= 0 || size.ScalePercent < 0 || (size.ScalePercent > 0 && (size.ScalePercent < 100 || size.ScalePercent > 500)) {
		return displayMode{}, false
	}
	size = size.WithScaleProtection()
	compareHeight := func(m displayMode) bool {
		clientAspect := int64(size.Width) * int64(m.pixelHeight)
		modeAspect := int64(m.pixelWidth) * int64(size.Height)
		return clientAspect > modeAspect || (clientAspect == modeAspect && size.Height > size.Width)
	}
	score := func(m displayMode, heightFirst bool) [8]float64 {
		width, height := m.pixelWidth, m.pixelHeight
		targetW, targetH := float64(size.Width), float64(size.Height)
		if size.ScalePercent > 0 {
			width, height = m.width, m.height
			targetW *= 100 / float64(size.ScalePercent)
			targetH *= 100 / float64(size.ScalePercent)
		}
		widthError := math.Abs(float64(width) - targetW)
		heightError := math.Abs(float64(height) - targetH)
		primary, secondary := widthError, heightError
		if heightFirst {
			primary, secondary = heightError, widthError
		}
		refreshRate := current.refreshRate
		if size.RefreshRate > 0 {
			refreshRate = float64(size.RefreshRate)
		}
		orientation, scale, changed := 0., 0., 0.
		if (m.pixelWidth < m.pixelHeight) != (size.Width < size.Height) {
			orientation = 1
		}
		if current.width > 0 && current.height > 0 &&
			(m.pixelWidth*current.width != current.pixelWidth*m.width || m.pixelHeight*current.height != current.pixelHeight*m.height) {
			scale = 1
		}
		if m.width != current.width || m.height != current.height || m.pixelWidth != current.pixelWidth || m.pixelHeight != current.pixelHeight || m.refreshRate != current.refreshRate {
			changed = 1
		}
		pixelPrimary, pixelSecondary := 0., 0.
		if size.ScalePercent > 0 {
			// Eligible modes already have enough pixels on the fitted edge.
			// For the same logical desktop, minimize capture resampling.
			pixelPrimary = math.Abs(float64(m.pixelWidth - size.Width))
			pixelSecondary = math.Abs(float64(m.pixelHeight - size.Height))
			if heightFirst {
				pixelPrimary, pixelSecondary = pixelSecondary, pixelPrimary
			}
		}
		return [8]float64{
			primary, secondary,
			orientation, math.Abs(m.refreshRate - refreshRate), pixelPrimary, pixelSecondary, scale, changed,
		}
	}
	var best displayMode
	var bestScore [8]float64
	found := false
	for _, m := range modes {
		if m.width < 2 || m.height < 2 || m.pixelWidth < 2 || m.pixelHeight < 2 || m.pixelWidth > 8192 || m.pixelHeight > 8192 {
			continue
		}
		// Below 1080 rows, enforce a native 1x mode as well as 100% UI
		// scaling. A sharper HiDPI source must not override this protection.
		if size.Height < 1080 && (m.width != m.pixelWidth || m.height != m.pixelHeight) {
			continue
		}
		heightFirst := compareHeight(m)
		if (heightFirst && m.pixelHeight < size.Height) || (!heightFirst && m.pixelWidth < size.Width) {
			continue
		}
		s := score(m, heightFirst)
		better := !found
		if found {
			for i := range s {
				if s[i] != bestScore[i] {
					better = s[i] < bestScore[i]
					break
				}
			}
		}
		if better {
			best, bestScore, found = m, s, true
		}
	}
	return best, found
}
