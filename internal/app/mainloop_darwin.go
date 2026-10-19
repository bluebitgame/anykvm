//go:build darwin

package app

import (
	"anykvm/internal/capture"
	"anykvm/internal/viewer"
)

func runApplicationMain(cfg Config, fn func()) {
	// SDL must own the original main thread; headless mode uses the native event loop.
	if cfg.Viewer && viewer.Available {
		fn()
		return
	}
	capture.RunWithMainLoop(fn)
}
