package gamestream

import (
	"context"
	"net"
	"time"
)

// sourceFramePacer prevents buffered compressed input from becoming an unlimited
// frame burst. It preserves source PTS spacing and caps catch-up at the negotiated
// frame rate; dependent pictures cannot safely be coalesced like raw captures.
// Coalesced raw input passes a zero captured time for rate limiting alone, so
// dropped input intervals do not turn encoder startup into recurring waits.
type sourceFramePacer struct {
	fps                    int
	captured, anchor, last time.Time
}

func (p *sourceFramePacer) reserve(captured, now time.Time) time.Time {
	due := now
	if !captured.IsZero() {
		if p.captured.IsZero() {
			p.captured, p.anchor = captured, now
		}
		due = p.anchor.Add(captured.Sub(p.captured))
		// A quiet resume or a clock adjustment must not schedule a new picture
		// far into the future or play back a gap which already elapsed locally.
		if due.Before(now.Add(-250*time.Millisecond)) || due.After(now.Add(time.Second)) {
			p.captured, p.anchor, due = captured, now, now
		}
	}
	if due.Before(now) {
		due = now
	}
	if !p.last.IsZero() && p.fps > 0 {
		if earliest := p.last.Add(time.Second / time.Duration(p.fps)); due.Before(earliest) {
			due = earliest
		}
	}
	p.last = due
	return due
}

// Match Sunshine's separation of encoder bitrate and network burst pacing.
// The zero value uses its ~800 Mbps burst ceiling. One video sender owns this
// state across frames and encoder restarts; it must not be shared concurrently.
type videoPacer struct {
	mbps int
	next time.Time
}

func (p *videoPacer) bytesPerSecond() int64 {
	mbps := p.mbps
	if mbps <= 0 {
		mbps = 800
	}
	return int64(mbps) * 1000000 / 8
}

func (p *videoPacer) duration(bytes int) time.Duration {
	return time.Duration(int64(bytes) * int64(time.Second) / p.bytesPerSecond())
}

func (p *videoPacer) reserve(now time.Time, bytes int) time.Time {
	start := now
	if p.next.After(start) {
		start = p.next
	}
	p.next = start.Add(p.duration(bytes))
	return start
}

func waitVideoPacing(ctx context.Context, due time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay := time.Until(due); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	return ctx.Err()
}

func (p *videoPacer) send(ctx context.Context, conn *net.UDPConn, peer *net.UDPAddr, packets [][]byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	total := 0
	for _, packet := range packets {
		total += len(packet) // Includes FEC shards and encryption/header bytes.
	}
	if total == 0 {
		return nil
	}
	start := p.reserve(time.Now(), total)
	sent := 0
	groupLimit := int(p.bytesPerSecond() / 1000)
	for i := 0; i < len(packets); {
		if err := waitVideoPacing(ctx, start.Add(p.duration(sent))); err != nil {
			return err
		}
		groupBytes := 0
		for i < len(packets) {
			packet := packets[i]
			// Allow one oversized datagram, but never add another past the 1 ms budget.
			if groupBytes > 0 && groupBytes+len(packet) > groupLimit {
				break
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := conn.WriteToUDP(packet, peer); err != nil {
				return err
			}
			sent += len(packet)
			groupBytes += len(packet)
			i++
		}
	}
	return nil
}
