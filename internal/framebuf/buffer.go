// Package framebuf shares immutable media bytes across asynchronous consumers.
package framebuf

import "sync/atomic"

// Buffer owns one reference on creation. Bytes must remain immutable until the
// last reference is released. Retain requires an existing live reference;
// every retained reference must be released exactly once.
type Buffer struct {
	data []byte
	refs atomic.Int64
	free func()
}

func New(data []byte, free func()) *Buffer {
	b := &Buffer{data: data, free: free}
	b.refs.Store(1)
	return b
}

// Acquire retains owner when supplied; callers keep their original data view.
// Without an owner it copies legacy reusable capture data once.
func Acquire(data []byte, owner *Buffer) *Buffer {
	if owner != nil {
		return owner.Retain()
	}
	return New(append([]byte(nil), data...), nil)
}

func (b *Buffer) Bytes() []byte { return b.data }

func (b *Buffer) Retain() *Buffer {
	if b.refs.Add(1) <= 1 {
		panic("framebuf: retain after release")
	}
	return b
}

func (b *Buffer) Release() {
	if b == nil {
		return
	}
	n := b.refs.Add(-1)
	if n < 0 {
		panic("framebuf: double release")
	}
	if n == 0 && b.free != nil {
		b.free()
	}
}
