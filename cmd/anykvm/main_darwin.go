//go:build darwin

package main

import "runtime"

// AppKit and SDL require the original OS main thread.
func init() { runtime.LockOSThread() }
