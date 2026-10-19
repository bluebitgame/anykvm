//go:build darwin && cgo

package airplay

import "anykvm/internal/videodec"

// Retain the internal name for native-backend assertions.
type systemVideoDecoder = videodec.SystemDecoder
