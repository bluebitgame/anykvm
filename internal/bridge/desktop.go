package bridge

import (
	"anykvm/internal/capture"
	"fmt"
	"log"
)

// ResizeRDPDesktop is called only for the currently controlling RDP session.
// Restarting capture produces an independent frame; RDPGFX resets its surfaces
// when that frame's output dimensions change.
func (b *Bridge) ResizeRDPDesktop(width, height int) error {
	return b.ResizeRDPDesktopWithOptions(capture.DesktopSize{Width: width, Height: height})
}

// ResizeRDPDesktopWithOptions handles resolution and DPI-only updates alike.
func (b *Bridge) ResizeRDPDesktopWithOptions(size capture.DesktopSize) error {
	if size.Width < 200 || size.Height < 200 || size.Width > 8192 || size.Height > 8192 || size.Width%2 != 0 || size.ScalePercent < 0 || (size.ScalePercent > 0 && (size.ScalePercent < 100 || size.ScalePercent > 500)) {
		return fmt.Errorf("invalid RDP desktop size %+v", size)
	}
	size = size.WithScaleProtection()
	b.captureMu.Lock()
	defer b.captureMu.Unlock()
	b.sessionMu.Lock()
	active := b.activeProto == "rdp"
	b.sessionMu.Unlock()
	if !active {
		return fmt.Errorf("RDP no longer controls capture")
	}
	if b.desktopSize != nil && *b.desktopSize == size {
		return nil
	}
	// Nonlocal sources retain their own geometry. Never resize the AnyKVM host
	// on behalf of a capture card or upstream remote desktop.
	m := b.activeMachine()
	dev := b.activeDevice()
	_, local := dev.(capture.DesktopConfigurer)
	_, scaled := dev.(capture.OutputSizeConfigurer)
	outputChanged := b.desktopSize == nil || b.desktopSize.Width != size.Width || b.desktopSize.Height != size.Height
	if m == nil || (!m.AutoResolution || !local) && (!scaled || !outputChanged) {
		b.desktopSize = &size
		return nil
	}
	b.stopCaptureLocked()
	b.desktopSize = &size
	b.startCaptureLocked()
	return nil
}

func (b *Bridge) restoreDesktopLocked(m *MachineSession) {
	if desktop, ok := m.Capture.(capture.DesktopConfigurer); ok {
		if err := desktop.RestoreDesktopSize(); err != nil {
			log.Printf("bridge: restore desktop for %q: %v", m.Name, err)
		}
	}
}

func (b *Bridge) restoreDesktopsLocked() {
	for _, m := range b.machines {
		b.restoreDesktopLocked(m)
	}
}
