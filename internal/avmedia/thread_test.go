//go:build ffmpeg && cgo

package avmedia

import "testing"

func TestMediaThreadLifecycle(t *testing.T) {
	worker := newMediaThread()
	count := 0
	for i := 0; i < 10; i++ {
		worker.call(func() { count++ })
	}
	worker.close()
	if count != 10 {
		t.Fatalf("lost calls: %d", count)
	}
	select {
	case <-worker.done:
	default:
		t.Fatal("worker still running")
	}
	var direct *mediaThread
	direct.call(func() { count++ })
	direct.close()
	if count != 11 {
		t.Fatal("direct fallback did not run")
	}
}
