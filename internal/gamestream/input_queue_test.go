package gamestream

import (
	"context"
	"encoding/binary"
	"testing"
	"time"
)

func relativePacket(x, y int) []byte {
	p := make([]byte, 12)
	binary.BigEndian.PutUint32(p, 8)
	binary.LittleEndian.PutUint32(p[4:], 7)
	binary.BigEndian.PutUint16(p[8:], uint16(int16(x)))
	binary.BigEndian.PutUint16(p[10:], uint16(int16(y)))
	return p
}
func TestInputQueueMergesWithoutCrossingButtons(t *testing.T) {
	q := &inputQueue{wake: make(chan struct{}, 1)}
	button := []byte{0, 0, 0, 5, 8, 0, 0, 0, 1}
	for _, p := range [][]byte{relativePacket(3, 4), relativePacket(-2, 5), button, relativePacket(7, 8)} {
		if err := q.enqueue(p); err != nil {
			t.Fatal(err)
		}
	}
	if len(q.packets) != 3 || q.merged != 1 {
		t.Fatalf("queue=%d merged=%d", len(q.packets), q.merged)
	}
	p := q.packets[0].data
	if int16(binary.BigEndian.Uint16(p[8:])) != 1 || int16(binary.BigEndian.Uint16(p[10:])) != 9 {
		t.Fatal("relative movement lost")
	}
	if mergeMouseMotion(relativePacket(32767, 0), relativePacket(1, 0)) {
		t.Fatal("overflow wrapped")
	}
}
func TestInputQueueReceiverDoesNotWaitForInjection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := &inputQueue{wake: make(chan struct{}, 1), started: time.Now()}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() { q.run(ctx, func([]byte) error { close(entered); <-release; return nil }); close(done) }()
	if err := q.enqueue(relativePacket(1, 1)); err != nil {
		t.Fatal(err)
	}
	<-entered
	queued := make(chan error, 1)
	go func() { queued <- q.enqueue(relativePacket(2, 2)) }()
	select {
	case err := <-queued:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(time.Second):
		t.Error("receiver blocked on OS injection")
	}
	cancel()
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}
func TestInputQueueBoundedAndNewestAbsolute(t *testing.T) {
	q := &inputQueue{wake: make(chan struct{}, 1)}
	p := make([]byte, 18)
	binary.BigEndian.PutUint32(p, 14)
	binary.LittleEndian.PutUint32(p[4:], 5)
	if err := q.enqueue(p); err != nil {
		t.Fatal(err)
	}
	p[9] = 99
	if err := q.enqueue(p); err != nil {
		t.Fatal(err)
	}
	if len(q.packets) != 1 || q.packets[0].data[9] != 99 {
		t.Fatal("absolute motion not replaced")
	}
	button := []byte{0, 0, 0, 5, 8, 0, 0, 0, 1}
	for len(q.packets) < 512 {
		if err := q.enqueue(button); err != nil {
			t.Fatal(err)
		}
	}
	if q.enqueue(button) == nil {
		t.Fatal("queue grew without bound")
	}
}
