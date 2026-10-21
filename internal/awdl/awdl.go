// Package awdl suspends Apple's peer-to-peer Wi-Fi interface while streaming.
package awdl

import (
	"sync"
	"time"

	"anykvm/internal/logging"
)

type networkInterface interface {
	IsUp() (bool, error)
	SetUp(bool) error
}

// Guard manages AWDL on macOS. Other platforms return nil from New.
// Commands run on a worker so client callbacks never wait for sudo or ifconfig.
type Guard struct {
	mu     sync.Mutex
	active bool
	closed bool
	wake   chan struct{}
	done   chan struct{}
}

// New starts an idle guard; it does not change the interface until SetActive.
func New() *Guard {
	iface := newSystemInterface()
	if iface == nil {
		return nil
	}
	return newGuard(iface, 2*time.Second)
}

func newGuard(iface networkInterface, interval time.Duration) *Guard {
	g := &Guard{wake: make(chan struct{}, 1), done: make(chan struct{})}
	go g.run(iface, interval)
	return g
}

// SetActive is safe to call while holding a connection-count lock. Updates are
// coalesced so protocol takeovers do not queue obsolete interface changes.
func (g *Guard) SetActive(active bool) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.active == active {
		return
	}
	g.active = active
	g.signalLocked()
}

func (g *Guard) signalLocked() {
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

// Close waits for the worker to restore an interface it successfully disabled.
func (g *Guard) Close() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.closed = true
	g.active = false
	g.signalLocked()
	g.mu.Unlock()
	<-g.done
}

func (g *Guard) run(iface networkInterface, interval time.Duration) {
	defer close(g.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var ticks <-chan time.Time // no interface polling while idle
	var haveOriginal, originalUp, changed bool
	var lastError string
	report := func(action string, err error) {
		message := action + ": " + err.Error()
		if message != lastError {
			logging.Infof("awdl: %s", message)
			lastError = message
		}
	}
	for {
		select {
		case <-g.wake:
		case <-ticks:
		}
		g.mu.Lock()
		active, closed := g.active, g.closed
		g.mu.Unlock()
		if active {
			ticks = ticker.C
			up, err := iface.IsUp()
			if err != nil {
				report("cannot read awdl0 state", err)
				continue
			}
			if !haveOriginal {
				haveOriginal, originalUp = true, up
			}
			if up {
				if err := iface.SetUp(false); err != nil {
					report("cannot disable awdl0 (see docs/capture.md#wi-fi-and-awdl for sudo setup)", err)
					continue
				}
				changed = true
				lastError = ""
				logging.Infof("awdl: disabled awdl0 while a client is connected")
			}
			continue
		}
		if haveOriginal && originalUp && changed {
			if err := iface.SetUp(true); err != nil {
				report("cannot restore awdl0 (manual recovery: sudo /sbin/ifconfig awdl0 up)", err)
				if !closed {
					ticks = ticker.C
					continue
				}
			} else {
				logging.Infof("awdl: restored awdl0 after the last client disconnected")
			}
		}
		haveOriginal, originalUp, changed = false, false, false
		lastError = ""
		ticks = nil
		if closed {
			return
		}
	}
}
