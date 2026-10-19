package gamestream

import (
	"context"
	"encoding/binary"
	"errors"
	"log"
	"sync"
	"time"
)

type queuedInput struct {
	data     []byte
	received time.Time
}

// Like Sunshine's input worker, the receiver only queues packets. OS injection
// runs without the queue lock, and adjacent motion can coalesce under load.
type inputQueue struct {
	mu                   sync.Mutex
	packets              []queuedInput
	wake                 chan struct{}
	received, merged     int
	lastArrival, started time.Time
	maxGap               time.Duration
}

func newInputQueue(ctx context.Context, state *inputState) *inputQueue {
	q := &inputQueue{wake: make(chan struct{}, 1), started: time.Now()}
	go q.run(ctx, state.handle)
	return q
}

func mergeMouseMotion(dst, src []byte) bool {
	if len(dst) < 8 || len(src) != len(dst) || !equalKind(dst, src) {
		return false
	}
	kind := binary.LittleEndian.Uint32(src[4:])
	switch {
	case (kind == 6 || kind == 7) && len(src) == 12:
		x := int(int16(binary.BigEndian.Uint16(dst[8:]))) + int(int16(binary.BigEndian.Uint16(src[8:])))
		y := int(int16(binary.BigEndian.Uint16(dst[10:]))) + int(int16(binary.BigEndian.Uint16(src[10:])))
		if x < -32768 || x > 32767 || y < -32768 || y > 32767 {
			return false
		}
		binary.BigEndian.PutUint16(dst[8:], uint16(int16(x)))
		binary.BigEndian.PutUint16(dst[10:], uint16(int16(y)))
		return true
	case kind == 5 && len(src) == 18:
		copy(dst, src) // newest absolute position, without crossing button/key events
		return true
	}
	return false
}
func equalKind(a, b []byte) bool {
	return binary.LittleEndian.Uint32(a[4:]) == binary.LittleEndian.Uint32(b[4:])
}
func (q *inputQueue) enqueue(p []byte) error {
	if len(p) < 8 || int(binary.BigEndian.Uint32(p)) != len(p)-4 {
		return errors.New("invalid input length")
	}
	now := time.Now()
	q.mu.Lock()
	defer q.mu.Unlock()
	q.received++
	if !q.lastArrival.IsZero() {
		q.maxGap = max(q.maxGap, now.Sub(q.lastArrival))
	}
	q.lastArrival = now
	n := len(q.packets)
	if n > 0 && mergeMouseMotion(q.packets[n-1].data, p) {
		q.merged++
		return nil
	}
	if n >= 512 {
		return errors.New("input queue overflow")
	}
	q.packets = append(q.packets, queuedInput{append([]byte(nil), p...), now})
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return nil
}
func (q *inputQueue) run(ctx context.Context, handle func([]byte) error) {
	restorePriority := prioritizeInputThread()
	defer restorePriority()
	var maxWait, maxDispatch time.Duration
	for {
		if ctx.Err() != nil {
			return
		}
		q.mu.Lock()
		if len(q.packets) == 0 {
			q.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-q.wake:
				continue
			}
		}
		p := q.packets[0]
		q.packets[0] = queuedInput{}
		q.packets = q.packets[1:]
		q.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		maxWait = max(maxWait, start.Sub(p.received))
		if err := handle(p.data); err != nil {
			log.Printf("gamestream: input: %v", err)
		}
		now := time.Now()
		maxDispatch = max(maxDispatch, now.Sub(start))
		q.mu.Lock()
		if elapsed := now.Sub(q.started); elapsed >= 5*time.Second {
			received, merged, gap, depth := q.received, q.merged, q.maxGap, len(q.packets)
			q.started, q.received, q.merged, q.maxGap = now, 0, 0, 0
			q.mu.Unlock()
			log.Printf("gamestream: input %.1f packets/s merged=%d queued=%d max_arrival_gap=%.1fms max_queue_wait=%.1fms max_dispatch=%.1fms", float64(received)/elapsed.Seconds(), merged, depth, float64(gap)/float64(time.Millisecond), float64(maxWait)/float64(time.Millisecond), float64(maxDispatch)/float64(time.Millisecond))
			maxWait, maxDispatch = 0, 0
		} else {
			q.mu.Unlock()
		}
	}
}
