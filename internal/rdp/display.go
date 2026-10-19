package rdp

import (
	"anykvm/internal/capture"
	"fmt"
	"log"
)

const dispChannel = 2

// MS-RDPBCGR/MS-RDPEDISP require the scale pair to be ignored unless both
// values are valid. DeviceScaleFactor describes client device characteristics;
// DesktopScaleFactor is the requested UI percentage and is not multiplied by it.
func desktopScalePercent(desktop, device uint32) int {
	if desktop < 100 || desktop > 500 || (device != 100 && device != 140 && device != 180) {
		return 0
	}
	return int(desktop)
}

// Display Control is optional. Clients rejecting it retain their initial size.
func (c *client) handleDisplayDVC(header byte, p []byte) error {
	if !c.dispCreated {
		return fmt.Errorf("%w: unrequested Display Control channel", errProtocol)
	}
	cmd := header >> 4
	switch cmd {
	case 1:
		if len(p) != 4 || c.dispOpen {
			return fmt.Errorf("%w: invalid Display Control create response", errProtocol)
		}
		if int32(le.Uint32(p)) < 0 {
			c.dispCreated = false
			return nil
		}
		c.dispOpen = true
		// One monitor, at most 8192x8192 pixels. Header order is Type, Length.
		return c.sendVC(join([]byte{0x30, dispChannel}, u32(5), u32(20), u32(1), u32(8192), u32(8192)))
	case 2, 3, 6, 7:
		if !c.dispOpen {
			return fmt.Errorf("%w: Display Control data before create response", errProtocol)
		}
		if cmd == 2 || cmd == 6 {
			n, data, err := dvcInteger(p, (header>>2)&3)
			if err != nil || n == 0 || n > 56 || c.dispTotal != 0 {
				return fmt.Errorf("%w: invalid Display Control fragment size", errProtocol)
			}
			c.dispTotal, c.dispBuf = int(n), c.dispBuf[:0]
			p = data
		}
		if cmd == 6 || cmd == 7 {
			if c.dvcVersion != 3 {
				return fmt.Errorf("%w: compressed Display Control without DVC v3", errProtocol)
			}
			var err error
			p, err = c.dispCompression.decode(p)
			if err != nil {
				return err
			}
		}
		if c.dispTotal == 0 {
			return c.handleDisplayLayout(p)
		}
		if len(p) > c.dispTotal-len(c.dispBuf) {
			return fmt.Errorf("%w: oversized Display Control fragment", errProtocol)
		}
		c.dispBuf = append(c.dispBuf, p...)
		if len(c.dispBuf) != c.dispTotal {
			return nil
		}
		c.dispTotal = 0
		return c.handleDisplayLayout(c.dispBuf)
	case 4:
		if len(p) != 0 {
			return errProtocol
		}
		c.dispOpen, c.dispCreated = false, false
		c.dispTotal, c.dispBuf = 0, nil
		return nil
	default:
		return fmt.Errorf("%w: unsupported Display Control command", errProtocol)
	}
}

func parseDisplayLayout(p []byte) (capture.DesktopSize, error) {
	// A single primary monitor at (0,0); no physical rotation is requested.
	if len(p) != 56 || le.Uint32(p) != 2 || le.Uint32(p[4:]) != 56 || le.Uint32(p[8:]) != 40 || le.Uint32(p[12:]) != 1 {
		return capture.DesktopSize{}, fmt.Errorf("%w: single-monitor Display Control layout required", errProtocol)
	}
	monitor := p[16:]
	if le.Uint32(monitor) != 1 || le.Uint32(monitor[4:]) != 0 || le.Uint32(monitor[8:]) != 0 {
		return capture.DesktopSize{}, fmt.Errorf("%w: primary monitor must start at (0,0)", errProtocol)
	}
	w, h := le.Uint32(monitor[12:]), le.Uint32(monitor[16:])
	if w < 200 || w > 8192 || w%2 != 0 || h < 200 || h > 8192 {
		return capture.DesktopSize{}, fmt.Errorf("%w: invalid Display Control dimensions", errProtocol)
	}
	// Physical size and orientation hints do not rotate the local display.
	return capture.DesktopSize{Width: int(w), Height: int(h), ScalePercent: desktopScalePercent(le.Uint32(monitor[32:]), le.Uint32(monitor[36:]))}, nil
}

func (c *client) handleDisplayLayout(p []byte) error {
	size, err := parseDisplayLayout(p)
	if err != nil {
		return err
	}
	if size.ScalePercent == 0 {
		size.ScalePercent = c.data.scalePercent
		if c.pendingDesktop != nil {
			size.ScalePercent = c.pendingDesktop.ScalePercent
		}
	}
	if !c.ready {
		c.pendingDesktop = &size
		return nil
	}
	c.s.mu.Lock()
	active := c.s.active == c && !c.s.closed
	c.s.mu.Unlock()
	if !active {
		return nil
	}
	c.data.width, c.data.height = size.Width, size.Height
	c.data.scalePercent = size.ScalePercent
	c.setOutputSize(size.Width, size.Height)
	c.desktopMu.Lock()
	c.desktopUpdate = &size
	c.desktopMu.Unlock()
	select {
	case c.desktopWake <- struct{}{}:
	default:
	}
	return nil
}

// Native display changes can take seconds. Keep them off the protocol reader so
// input, frame acknowledgments and disconnects continue during capture startup
// and resizing. Coalesce queued layouts while a previous switch is in progress.
func (c *client) desktopLoop(initial capture.DesktopSize) {
	c.lifeMu.Lock()
	select {
	case <-c.done:
		c.lifeMu.Unlock()
		return
	default:
	}
	c.sessionStarted = true
	if c.s.cfg.OnSessionStart != nil {
		c.s.cfg.OnSessionStart(c.codec.String(), initial)
	} else if c.s.cfg.OnConnected != nil {
		c.s.cfg.OnConnected(c.codec.String())
	}
	c.input.activate(c.s.cfg.HID)
	c.lifeMu.Unlock()
	for {
		select {
		case <-c.done:
			return
		case <-c.desktopWake:
		}
		c.lifeMu.Lock()
		select {
		case <-c.done:
			c.lifeMu.Unlock()
			return
		default:
		}
		c.desktopMu.Lock()
		size := c.desktopUpdate
		c.desktopUpdate = nil
		c.desktopMu.Unlock()
		c.s.mu.Lock()
		active := c.s.active == c && !c.s.closed
		c.s.mu.Unlock()
		if active && size != nil && c.s.cfg.OnDesktopResize != nil {
			if err := c.s.cfg.OnDesktopResize(*size); err != nil {
				log.Printf("rdp: desktop resize: %v", err)
			}
		}
		c.lifeMu.Unlock()
	}
}
