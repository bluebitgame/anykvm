//go:build ffmpeg && cgo

package avmedia

import "runtime"

// mediaThread owns thread-affine capture APIs such as DirectShow COM objects.
// Session's operation mutex serializes call/close; interrupt remains out of band.
type mediaThread struct {
	jobs chan func()
	done chan struct{}
}

func newMediaThread() *mediaThread {
	t := &mediaThread{jobs: make(chan func()), done: make(chan struct{})}
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(t.done)
		for job := range t.jobs {
			job()
		}
	}()
	return t
}
func (t *mediaThread) call(job func()) {
	if t == nil {
		job()
		return
	}
	done := make(chan struct{})
	t.jobs <- func() { defer close(done); job() }
	<-done
}
func (t *mediaThread) close() {
	if t != nil {
		close(t.jobs)
		<-t.done
	}
}
