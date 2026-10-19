//go:build !viewer

package viewer

import "anykvm/internal/bridge"

// Available reports whether the local GUI was included at build time.
const Available = false

// Run is a no-op in builds without local viewer support.
func Run(*bridge.Bridge, string, <-chan struct{}) error { return nil }
