package framebuf

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestSharedReleaseAndLegacyIsolation(t *testing.T) {
	data := []byte{1, 2, 3}
	var freed atomic.Int32
	b := New(data, func() { freed.Add(1) })
	var wg sync.WaitGroup
	for range 32 {
		held := Acquire(data, b)
		if &held.Bytes()[0] != &data[0] {
			t.Fatal("owned data copied")
		}
		wg.Add(1)
		go func() { defer wg.Done(); held.Release() }()
	}
	wg.Wait()
	if freed.Load() != 0 {
		t.Fatal("released with live producer")
	}
	b.Release()
	if freed.Load() != 1 {
		t.Fatal("release callback did not run exactly once")
	}
	copied := Acquire(data, nil)
	defer copied.Release()
	data[0] = 9
	if copied.Bytes()[0] != 1 {
		t.Fatal("legacy reusable storage escaped")
	}
}
