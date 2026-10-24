package logging

import (
	"runtime"
	"time"
)

// WatchStall reports blocked Go and cgo callers without interrupting resource
// teardown. The caller stops the timer when its operation completes.
func WatchStall(operation string, delay time.Duration) func() {
	timer := time.AfterFunc(delay, func() {
		stacks := make([]byte, 1<<20)
		n := runtime.Stack(stacks, true)
		Infof("%s stalled for %s; goroutine stacks:\n%s", operation, delay, stacks[:n])
	})
	return func() { timer.Stop() }
}
