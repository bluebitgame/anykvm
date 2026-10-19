//go:build viewer

package viewer

import (
	"log"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
)

// Available reports whether the local GUI was included at build time.
const Available = true

// Run opens the local viewer on the caller's locked OS main thread.
// The stop channel requests shutdown; ErrTakenOver means a remote client took control.
func Run(b *bridge.Bridge, mouseMode string, stop <-chan struct{}) error {
	dev := NewDevice()
	ffPath, _ := capture.ResolveFFmpeg()
	g := New(dev, b, ffPath, mouseMode, ScrollNone)
	dev.Audio = g.AudioCallback()
	started := false
	dev.Start = func() { started = true; b.StartViewer() }
	b.SetViewer(dev, dev)
	defer dev.Close()
	defer func() {
		if started {
			b.OnClientDisconnected()
		}
	}()
	go func() {
		select {
		case <-stop:
		case <-dev.Done():
		}
		g.Stop()
	}()
	log.Print("AnyKVM: starting viewer; use hotkey_mod + 1..9/0 to switch targets")
	err := g.Run()
	if dev.TakenOver() {
		return ErrTakenOver
	}
	return err
}
