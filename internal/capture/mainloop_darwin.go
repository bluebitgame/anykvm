//go:build darwin

package capture

/*
#cgo LDFLAGS: -framework Foundation
#include "mainloop_darwin.h"
*/
import "C"

import "runtime"

// RunWithMainLoop runs a CLI server on a worker while the original OS main
// thread processes AppKit and display notifications. The caller must pin the
// main goroutine to that thread during init, before Go's scheduler can move it.
func RunWithMainLoop(fn func()) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if C.bk_mainloop_prepare() == 0 {
		panic("macOS event loop requires the OS main thread")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer C.bk_mainloop_stop()
		defer shutdownDeferredClipboardFiles()
		fn()
	}()
	C.bk_mainloop_run()
	<-done
}
