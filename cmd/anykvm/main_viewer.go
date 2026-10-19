//go:build viewer && !darwin

package main

import "runtime"

// Keep SDL window creation and event processing on the same OS thread.
func init() { runtime.LockOSThread() }
