package netstats

import (
	"context"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"anykvm/internal/logging"
)

type writeFixture struct {
	net.Conn
	partial bool
}

type reportLog chan string

func (c reportLog) Write(p []byte) (int, error) { c <- string(p); return len(p), nil }

func TestGlobalReportEveryFiveSecondsWithoutFinalFlush(t *testing.T) {
	messages := make(reportLog, 4)
	logging.Configure(false, true)
	t.Cleanup(func() { logging.Configure(false, false) })
	previous := log.Writer()
	log.SetOutput(messages)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var active atomic.Bool
	go func() { defer close(done); Run(ctx, active.Load) }()
	defer func() { cancel(); <-done; log.SetOutput(previous) }()
	select {
	case line := <-messages:
		t.Fatalf("idle application reported uplink: %s", line)
	case <-time.After(5500 * time.Millisecond):
	}
	active.Store(true)
	select {
	case line := <-messages:
		if !strings.Contains(line, "AnyKVM: uplink=") {
			t.Fatalf("unexpected reporting interval or label: %s", line)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("no global uplink report after five seconds")
	}
	active.Store(false)
	select {
	case line := <-messages:
		t.Fatalf("disconnected application reported uplink: %s", line)
	case <-time.After(5500 * time.Millisecond):
	}
	Add(100)
	cancel()
	<-done
	if len(messages) != 0 {
		t.Fatal("shutdown produced an extra report")
	}
}

func (c *writeFixture) Write(p []byte) (int, error) {
	if c.partial {
		return 3, io.ErrClosedPipe
	}
	return len(p), nil
}

func TestGlobalTXCountsActualWriteResults(t *testing.T) {
	before := Total()
	Add(0)
	Add(-1)
	n, err := WrapConn(&writeFixture{partial: true}).Write(make([]byte, 100))
	if n != 3 || err != io.ErrClosedPipe {
		t.Fatal("write result changed")
	}
	_, _ = WrapConn(&writeFixture{}).Write(make([]byte, 17))
	if got := Total() - before; got != 20 {
		t.Fatalf("actual sent bytes=%d, want 20", got)
	}
}

func TestGlobalTXAggregatesConcurrentSenders(t *testing.T) {
	before := Total()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := WrapConn(&writeFixture{})
			for range 100 {
				_, _ = c.Write(make([]byte, 64))
				Add(32) // UDP senders share the same counter.
			}
		}()
	}
	wg.Wait()
	if got := Total() - before; got != 8*100*96 {
		t.Fatalf("shared sent bytes=%d", got)
	}
}
