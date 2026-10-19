package hid

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	ch9329Address     = 0x00
	ch9329InfoCmd     = 0x01
	ch9329KeyboardCmd = 0x02
	ch9329AbsoluteCmd = 0x04
	ch9329RelativeCmd = 0x05
	ch9329AbsoluteMax = 4095
	inputAbsoluteMax  = 32767
)

// ch9329Base holds the shared serial connection and implements the keyboard,
// button, wheel and Close portions of hidi. CH9329 has no persistent mouse
// mode: absolute and relative reports are selected by their command byte.
type ch9329Base struct {
	mu       sync.Mutex // protects mouse state and report ordering
	f        io.ReadWriteCloser
	replies  chan []byte
	done     chan struct{}
	absolute bool
	btnState byte
	x        int // CH9329 absolute range (0..4095)
	y        int
}

func newCH9329Base(f io.ReadWriteCloser, absolute bool) *ch9329Base {
	b := &ch9329Base{f: f, absolute: absolute, replies: make(chan []byte, 16), done: make(chan struct{})}
	go b.readResponses()
	return b
}

func (b *ch9329Base) Close() error { return b.f.Close() }

// Keep reading across fragmented replies and serial noise. Input commands
// wait for their acknowledgement before sending another report.
func (b *ch9329Base) readResponses() {
	defer close(b.done)
	buf := make([]byte, 128)
	var pending []byte
	for {
		n, err := b.f.Read(buf)
		pending = append(pending, buf[:n]...)
		for len(pending) >= 5 {
			if pending[0] != 0x57 || pending[1] != 0xAB || pending[4] > 64 {
				pending = pending[1:]
				continue
			}
			length := 6 + int(pending[4])
			if len(pending) < length {
				break
			}
			if ch9329Checksum(pending[:length-1]) != pending[length-1] {
				pending = pending[1:]
				continue
			}
			reply := append([]byte(nil), pending[:length]...)
			pending = pending[length:]
			select {
			case b.replies <- reply:
			default: // Unsolicited status updates must not block the reader.
			}
		}
		if err != nil {
			return
		}
		if n == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func (b *ch9329Base) writeFrame(command byte, data ...byte) error {
	// mu is held by every input method, including while awaiting the reply.
	for len(b.replies) > 0 {
		<-b.replies
	}
	frame := ch9329Frame(command, data...)
	n, err := b.f.Write(frame)
	if err != nil {
		return err
	}
	if n != len(frame) {
		return io.ErrShortWrite
	}
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case reply := <-b.replies:
			if reply[2] != ch9329Address || reply[3]&0x3f != command {
				continue
			}
			if reply[4] != 1 {
				continue
			}
			if reply[3] != command|0x80 || reply[5] != 0 {
				return fmt.Errorf("ch9329: command 0x%02x failed with status 0x%02x", command, reply[5])
			}
			return nil
		case <-b.done:
			return fmt.Errorf("ch9329: connection closed awaiting command 0x%02x", command)
		case <-timer.C:
			return fmt.Errorf("ch9329: timed out awaiting command 0x%02x", command)
		}
	}
}

func (b *ch9329Base) KeyDown(keycode uint8) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Standard eight-byte boot-keyboard report: modifier, reserved, six keys.
	return b.writeFrame(ch9329KeyboardCmd, 0, 0, keycode, 0, 0, 0, 0, 0)
}

func (b *ch9329Base) KeyUp() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.writeFrame(ch9329KeyboardCmd, 0, 0, 0, 0, 0, 0, 0, 0)
}

func (b *ch9329Base) KeyRelease(keycode uint8) error { return b.KeyUp() }

func (b *ch9329Base) MouseButton(button int, down bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	var btn byte
	switch button {
	case 1:
		btn = 1 // left
	case 2:
		btn = 2 // right
	case 3:
		btn = 4 // middle
	default:
		return nil
	}
	if down {
		b.btnState |= btn
	} else {
		b.btnState &^= btn
	}
	return b.mouseReport(0, 0, 0)
}

func (b *ch9329Base) MouseWheel(delta int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.mouseReport(0, 0, delta)
}

// mouseReport requires mu to be held through the write: a report built before
// a button transition must never be sent after that transition's report.
func (b *ch9329Base) mouseReport(dx, dy, wheel int) error {
	if b.absolute {
		return b.writeFrame(ch9329AbsoluteCmd,
			0x02, b.btnState,
			byte(b.x), byte(b.x>>8), byte(b.y), byte(b.y>>8),
			byte(int8(clamp(wheel, -128, 127))),
		)
	}
	return b.writeFrame(ch9329RelativeCmd,
		0x01, b.btnState,
		byte(int8(clamp(dx, -128, 127))), byte(int8(clamp(dy, -128, 127))),
		byte(int8(clamp(wheel, -128, 127))),
	)
}

// ch9329AbsDevice uses the controller's 12-bit absolute coordinate range.
// Callers use the common USB-HID 0..32767 range, so map it before sending.
type ch9329AbsDevice struct {
	*ch9329Base
	relX int // tracked position for MouseMoveRel, in caller coordinates
	relY int
}

func (a *ch9329AbsDevice) MouseMove(x, y int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mouseMove(x, y)
}

func (a *ch9329AbsDevice) mouseMove(x, y int) error {
	a.relX = clamp(x, 0, inputAbsoluteMax)
	a.relY = clamp(y, 0, inputAbsoluteMax)
	a.x = ch9329Coordinate(a.relX)
	a.y = ch9329Coordinate(a.relY)
	return a.mouseReport(0, 0, 0)
}

// MouseMoveRel tracks the absolute position and sends an absolute update.
func (a *ch9329AbsDevice) MouseMoveRel(dx, dy int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mouseMove(a.relX+dx, a.relY+dy)
}

// ch9329RelDevice computes deltas from the absolute coordinates supplied by
// callers, then emits CH9329 relative reports.
type ch9329RelDevice struct {
	*ch9329Base
	lastX int
	lastY int
}

func (r *ch9329RelDevice) MouseMove(x, y int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	dx := x - r.lastX
	dy := y - r.lastY
	r.lastX = x
	r.lastY = y
	return r.mouseMoveRel(dx, dy)
}

func (r *ch9329RelDevice) MouseMoveRel(dx, dy int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mouseMoveRel(dx, dy)
}

// Split large deltas without losing distance or releasing a held button.
func (r *ch9329RelDevice) mouseMoveRel(dx, dy int) error {
	for {
		x, y := clamp(dx, -127, 127), clamp(dy, -127, 127)
		if err := r.mouseReport(x, y, 0); err != nil {
			return err
		}
		dx, dy = dx-x, dy-y
		if dx == 0 && dy == 0 {
			return nil
		}
	}
}

// ch9329Frame builds a command according to the CH9329 serial protocol:
// 57 AB address command data-length data checksum. The checksum is the low
// byte of the sum of every preceding byte, not an XOR checksum.
func ch9329Frame(command byte, data ...byte) []byte {
	frame := make([]byte, 0, 6+len(data))
	frame = append(frame, 0x57, 0xAB, ch9329Address, command, byte(len(data)))
	frame = append(frame, data...)
	return append(frame, ch9329Checksum(frame))
}

func ch9329Checksum(data []byte) byte {
	var sum byte
	for _, b := range data {
		sum += b
	}
	return sum
}

func ch9329Coordinate(value int) int {
	value = clamp(value, 0, inputAbsoluteMax)
	return value * ch9329AbsoluteMax / inputAbsoluteMax
}

// isCH9329InfoReply reports whether data contains a complete, checksummed
// response to the GET_INFO command. It tolerates leading serial noise.
func isCH9329InfoReply(data []byte) bool {
	for start := 0; start+6 <= len(data); start++ {
		if data[start] != 0x57 || data[start+1] != 0xAB || data[start+3] != ch9329InfoCmd|0x80 {
			continue
		}
		end := start + 6 + int(data[start+4])
		if end > len(data) {
			continue
		}
		if ch9329Checksum(data[start:end-1]) == data[end-1] {
			return true
		}
	}
	return false
}

func probeCH9329Connection(f serialReadWriter) bool {
	request := ch9329Frame(ch9329InfoCmd)
	deadline := time.Now().Add(300 * time.Millisecond)
	_ = f.SetWriteDeadline(deadline)
	if n, err := f.Write(request); err != nil || n != len(request) {
		return false
	}

	var received []byte
	buffer := make([]byte, 128)
	for time.Now().Before(deadline) {
		_ = f.SetReadDeadline(deadline)
		n, err := f.Read(buffer)
		if n > 0 {
			received = append(received, buffer[:n]...)
			if isCH9329InfoReply(received) {
				return true
			}
			// One frame is at most 6 header/checksum bytes plus 255 data bytes.
			if len(received) > 261 {
				received = received[len(received)-261:]
			}
		}
		if err != nil && err != io.EOF && !os.IsTimeout(err) && !isNetTimeout(err) {
			return false
		}
		if n == 0 {
			time.Sleep(5 * time.Millisecond)
		}
	}
	return false
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func findCH9329() (string, error) {
	patterns := []string{"/dev/tty.usbserial", "/dev/ttyUSB", "/dev/cu.usbserial"}
	for _, prefix := range patterns {
		entries, _ := os.ReadDir("/dev")
		for _, e := range entries {
			name := "/dev/" + e.Name()
			if strings.HasPrefix(name, prefix) {
				return name, nil
			}
		}
	}
	return "", fmt.Errorf("ch9329: no serial device found")
}
