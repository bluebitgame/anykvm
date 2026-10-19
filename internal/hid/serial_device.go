package hid

import (
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// ListBLE scans serial ports for compatible BLE HID dongles and returns
// discovered devices with their slot bond status.
func ListBLE() []BLEDeviceInfo {
	var result []BLEDeviceInfo
	for _, port := range serialHIDCandidates() {
		if info, ok := probeBLEPort(port); ok {
			result = append(result, info)
		}
	}
	return result
}

func probeBLEPort(port string) (BLEDeviceInfo, bool) {
	var info BLEDeviceInfo
	found := probeSerialPort(port, baud115200, func(rw serialReadWriter) bool {
		d := newSerialDeviceRW(rw)
		defer d.closeLink()
		if probeSerialDeviceWithTimeout(d, serialProbeReplyTimeout, serialProbeRetryDelay) != nil {
			return false
		}
		st := d.lastStatus()
		if st.Capabilities&capabilityBLE == 0 {
			return false
		}
		slotCount := st.SlotCount
		if slotCount == 0 {
			slotCount = 1
		}
		info = BLEDeviceInfo{Port: port, Name: serialModelName(st.Model), SlotCount: slotCount}
		if st.Slot >= 0 && st.Slot < len(info.Slots) {
			info.Slots[st.Slot] = BLEStatus{Bonded: st.Bonded}
		}
		return true
	})
	if !found {
		return BLEDeviceInfo{}, false
	}
	return info, found
}

func serialConnect(port string) (*serialHIDDevice, error) {
	f, err := openHIDSerialFile(port)
	if err != nil {
		return nil, fmt.Errorf("hid: open %s: %w", port, err)
	}
	if err := setControlTermios(f, baud115200); err != nil {
		f.Close()
		return nil, fmt.Errorf("hid: configure %s: %w", port, err)
	}
	_ = setSerialDTR(f)
	drainSerial(f, 300*time.Millisecond)
	time.Sleep(200 * time.Millisecond)

	d := newSerialDevice(f)
	if err := probeSerialDevice(d); err != nil {
		d.closeLink()
		return nil, fmt.Errorf("hid: probe %s: %w", port, err)
	}
	return d, nil
}

func probeSerialDevice(d *serialHIDDevice) error {
	return probeSerialDeviceWithTimeout(d, 1500*time.Millisecond, 150*time.Millisecond)
}

func probeSerialDeviceWithTimeout(d *serialHIDDevice, responseTimeout, retryDelay time.Duration) error {
	for attempt := 0; attempt < 3; attempt++ {
		st, err := d.profileStatus(responseTimeout)
		if err == nil && st.Version == serialProtocolVersion && st.Capabilities&capabilityKeyboard != 0 {
			if st.SlotCount < 0 || st.SlotCount > 4 ||
				(st.Capabilities&capabilityProfiles != 0 && st.SlotCount == 0) {
				return fmt.Errorf("hid: invalid profile capabilities")
			}
			return nil
		}
		if err == nil && st.Version != 0 && st.Version != serialProtocolVersion {
			return fmt.Errorf("hid: protocol version %d; flash AnyKVM v%d firmware", st.Version, serialProtocolVersion)
		}
		if attempt < 2 {
			time.Sleep(retryDelay)
		}
	}
	return fmt.Errorf("hid: AnyKVM v1 capability query timeout; firmware update may be required")
}

func drainSerial(f *os.File, maxWait time.Duration) {
	buf := make([]byte, 256)
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		_ = f.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		n, err := f.Read(buf)
		if err != nil {
			if os.IsTimeout(err) || (err == io.EOF && n == 0) {
				time.Sleep(time.Millisecond)
				continue
			}
			break
		}
		if n == 0 {
			time.Sleep(time.Millisecond)
			continue
		}
	}
	_ = f.SetReadDeadline(time.Time{})
}

type serialReadWriter interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Close() error
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

// serialHIDDevice talks to all AnyKVM v1 adapters over CDC or UART.
type serialHIDDevice struct {
	rw         serialReadWriter
	mu         sync.Mutex
	controlMu  sync.Mutex
	ackWait    chan [2]byte
	ackCommand byte

	done      chan struct{}
	closeOnce sync.Once
	events    chan BLEEvent

	stMu   sync.Mutex
	stWait []chan ProfileStatus
	last   ProfileStatus

	rxIdx int
	rxBuf [8]byte
}

func newSerialDevice(f *os.File) *serialHIDDevice {
	return newSerialDeviceRW(f)
}

func newSerialDeviceRW(rw serialReadWriter) *serialHIDDevice {
	n := &serialHIDDevice{
		rw:     rw,
		done:   make(chan struct{}),
		events: make(chan BLEEvent, 16),
	}
	go n.readLoop()
	return n
}

func (n *serialHIDDevice) Close() error {
	return n.closeDevice(true)
}
func (n *serialHIDDevice) closeLink() error { return n.closeDevice(false) }
func (n *serialHIDDevice) closeDevice(release bool) error {
	var err error
	n.closeOnce.Do(func() {
		if release {
			_ = n.sendCmd(0x10, [4]byte{})
		}
		close(n.done)
		err = n.rw.Close()
	})
	return err
}

// --- hidi interface ---------------------------------------------------

func (n *serialHIDDevice) KeyDown(code uint8) error {
	return n.sendCmd(0x11, [4]byte{code, 1, 0, 0})
}

func (n *serialHIDDevice) KeyUp() error {
	return n.sendCmd(0x10, [4]byte{})
}

func (n *serialHIDDevice) KeyRelease(code uint8) error {
	return n.sendCmd(0x11, [4]byte{code, 0, 0, 0})
}

func (n *serialHIDDevice) MouseMove(x, y int) error {
	x = clamp(x, 0, 32767)
	y = clamp(y, 0, 32767)
	return n.sendCmd(0x12, [4]byte{
		uint8((x >> 8) & 0xFF), uint8(x & 0xFF),
		uint8((y >> 8) & 0xFF), uint8(y & 0xFF),
	})
}

func (n *serialHIDDevice) MouseMoveRel(dx, dy int) error {
	for dx != 0 || dy != 0 {
		sx := int8(clamp(dx, -127, 127))
		sy := int8(clamp(dy, -127, 127))
		if err := n.sendCmd(0x15, [4]byte{uint8(sx), uint8(sy), 0, 0}); err != nil {
			return err
		}
		dx -= int(sx)
		dy -= int(sy)
	}
	return nil
}

func (n *serialHIDDevice) MouseButton(button int, down bool) error {
	var code byte
	switch button {
	case 1:
		code = 0x80
	case 2:
		code = 0x40
	case 3:
		code = 0x20
	default:
		return nil
	}
	if down {
		switch button {
		case 1:
			code |= 0x08
		case 2:
			code |= 0x04
		case 3:
			code |= 0x02
		}
	}
	return n.sendCmd(0x13, [4]byte{code, 0, 0, 0})
}

func (n *serialHIDDevice) MouseWheel(delta int) error {
	v := int8(clamp(delta, -127, 127))
	return n.sendCmd(0x14, [4]byte{0, uint8(v), 0, 0})
}

// --- BLEController interface ------------------------------------------

func (n *serialHIDDevice) ProfileSelect(slot int) error {
	if err := n.validateSlot(slot); err != nil {
		return err
	}
	return n.control(0x20, [4]byte{byte(slot), 0, 0, 0})
}

func (n *serialHIDDevice) ProfileClear(slot int) error {
	if err := n.validateSlot(slot); err != nil {
		return err
	}
	return n.control(0x21, [4]byte{byte(slot), 0, 0, 0})
}

func (n *serialHIDDevice) validateSlot(slot int) error {
	st := n.lastStatus()
	if st.Capabilities&capabilityProfiles == 0 {
		return fmt.Errorf("hid: device has no BLE profiles")
	}
	count := st.SlotCount
	if slot < 0 || slot >= count {
		return fmt.Errorf("ble: invalid slot %d (device has %d slots)", slot, count)
	}
	return nil
}

func (n *serialHIDDevice) ProfileAdvertise() error {
	if err := n.validateSlot(0); err != nil {
		return err
	}
	return n.control(0x23, [4]byte{})
}

func (n *serialHIDDevice) SetMouseMode(absolute bool) error {
	st := n.lastStatus()
	capability := byte(capabilityRelative)
	if absolute {
		capability = capabilityAbsolute
	}
	if st.Capabilities&capability == 0 {
		return fmt.Errorf("hid: requested mouse mode is unsupported")
	}
	mode := byte(0x10)
	if absolute {
		mode = 0x08
	}
	return n.control(0x04, [4]byte{mode, 0, 0, 0})
}

// configureSerialSession selects an optional profile slot and syncs mouse mode
// with the host (-mouse-mode). Skips SetMouseMode when the dongle already
// reports the requested mode to avoid unnecessary BLE disconnects.
func configureSerialSession(n *serialHIDDevice, absolute bool, slot int) error {
	capability := capabilityRelative
	if absolute {
		capability = capabilityAbsolute
	}
	if n.lastStatus().Capabilities&capability == 0 {
		return fmt.Errorf("hid: requested mouse mode is unsupported")
	}
	if slot >= 0 {
		if err := n.ProfileSelect(slot); err != nil {
			return fmt.Errorf("select slot %d: %w", slot, err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	want := "relative"
	if absolute {
		want = "absolute"
	}
	if st, err := n.ProfileStatus(); err == nil && st.MouseMode == want {
		return nil
	}
	if err := n.SetMouseMode(absolute); err != nil {
		return fmt.Errorf("set mouse mode: %w", err)
	}
	st, err := n.ProfileStatus()
	if err != nil {
		return err
	}
	if st.MouseMode != want {
		return fmt.Errorf("hid: mouse mode %s was not applied", want)
	}
	return n.KeyUp()
}

func (n *serialHIDDevice) ProfileEvents() <-chan BLEEvent {
	return n.events
}

func (n *serialHIDDevice) ProfileStatus() (ProfileStatus, error) {
	return n.profileStatus(1500 * time.Millisecond)
}

func (n *serialHIDDevice) profileStatus(timeout time.Duration) (ProfileStatus, error) {
	ch := make(chan ProfileStatus, 1)
	n.stMu.Lock()
	n.stWait = append(n.stWait, ch)
	n.stMu.Unlock()
	defer func() {
		n.stMu.Lock()
		n.cancelWaiter(ch)
		n.stMu.Unlock()
	}()

	if err := n.sendCmd(0x22, [4]byte{}); err != nil {
		return n.lastStatus(), err
	}

	select {
	case st := <-ch:
		return st, nil
	case <-time.After(timeout):
		return n.lastStatus(), fmt.Errorf("ble: status query timeout")
	case <-n.done:
		return n.lastStatus(), fmt.Errorf("ble: device closed")
	}
}

func (n *serialHIDDevice) lastStatus() ProfileStatus {
	n.stMu.Lock()
	defer n.stMu.Unlock()
	return n.last
}

func (n *serialHIDDevice) cancelWaiter(ch chan ProfileStatus) bool {
	for i, w := range n.stWait {
		if w == ch {
			n.stWait = append(n.stWait[:i], n.stWait[i+1:]...)
			return true
		}
	}
	return false
}

func (n *serialHIDDevice) Pair(slot int) error {
	if err := n.ProfileSelect(slot); err != nil {
		return err
	}
	time.Sleep(50 * time.Millisecond)
	if err := n.ProfileClear(slot); err != nil {
		return err
	}
	return n.ProfileAdvertise()
}

// --- frame write ------------------------------------------------------

func (n *serialHIDDevice) sendCmd(cmd byte, data [4]byte) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	frame := serialHIDFrame(cmd, data)

	_ = n.rw.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	written, err := n.rw.Write(frame)
	if err == nil && written != len(frame) {
		return io.ErrShortWrite
	}
	return err
}

// control waits for the device's result, including unsupported/busy errors.
func (n *serialHIDDevice) control(cmd byte, data [4]byte) error {
	n.controlMu.Lock()
	defer n.controlMu.Unlock()
	ch := make(chan [2]byte, 1)
	n.stMu.Lock()
	n.ackWait = ch
	n.ackCommand = cmd
	n.stMu.Unlock()
	defer func() { n.stMu.Lock(); n.ackWait = nil; n.stMu.Unlock() }()
	if err := n.sendCmd(cmd, data); err != nil {
		return err
	}
	select {
	case ack := <-ch:
		if ack[1] != 0 {
			return fmt.Errorf("hid: command 0x%02x failed (status %d)", cmd, ack[1])
		}
		return nil
	case <-n.done:
		return fmt.Errorf("hid: device closed")
	case <-time.After(3 * time.Second):
		return fmt.Errorf("hid: command 0x%02x acknowledgement timeout", cmd)
	}
}

// --- reply frame reader -----------------------------------------------

func (n *serialHIDDevice) readLoop() {
	defer close(n.events)
	buf := make([]byte, 256)

	for {
		select {
		case <-n.done:
			return
		default:
		}

		n.rw.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		nr, err := n.rw.Read(buf)
		if err != nil {
			if os.IsTimeout(err) || isNetTimeout(err) {
				continue
			}
			if err == io.EOF && nr == 0 {
				time.Sleep(5 * time.Millisecond)
				continue // Serial ports can return EOF for a bounded idle read.
			}
			return
		}

		for i := 0; i < nr; i++ {
			n.replyRxByte(buf[i])
		}
	}
}

func isNetTimeout(err error) bool {
	if netErr, ok := err.(net.Error); ok {
		return netErr.Timeout()
	}
	return false
}

func (n *serialHIDDevice) replyRxByte(b byte) {
	n.rxBuf[n.rxIdx] = b
	n.rxIdx++
	if n.rxIdx < len(n.rxBuf) {
		return
	}
	frame := n.rxBuf[:]
	crc := uint16(frame[6])<<8 | uint16(frame[7])
	if frame[0] == 0x33 && crc16modbus(frame[:6]) == crc {
		n.dispatchReply(frame[1], [4]byte{frame[2], frame[3], frame[4], frame[5]})
		n.rxIdx = 0
	} else {
		copy(n.rxBuf[:], n.rxBuf[1:])
		n.rxIdx--
	}
}

func (n *serialHIDDevice) dispatchReply(cmd byte, data [4]byte) {
	n.stMu.Lock()
	defer n.stMu.Unlock()
	switch cmd {
	case 0x83:
		n.last.Version = int(data[0])
		n.last.Capabilities = data[1]
		n.last.SlotCount = int(data[2])
		n.last.Model = data[3]
	case 0x84:
		if n.ackWait != nil && data[0] == n.ackCommand {
			select {
			case n.ackWait <- [2]byte{data[0], data[1]}:
			default:
			}
		}
	case 0x80:
		n.last.Ready = true
		n.last.SlotCount = int(data[0])
	case 0x81:
		n.last.Slot = int(data[0] & 0x03)
		n.last.Connected = data[0]&0x04 != 0
		n.last.Bonded = data[0]&0x08 != 0
		n.last.Advertising = data[0]&0x10 != 0
		if data[0]&0x20 != 0 {
			n.last.MouseMode = "absolute"
		} else {
			n.last.MouseMode = "relative"
		}
		if len(n.stWait) > 0 {
			ch := n.stWait[0]
			n.stWait = n.stWait[1:]
			select {
			case ch <- n.last:
			default:
			}
		}
	case 0x82:
		evtType := data[0]
		if evtType == 0x01 || evtType == 0x02 {
			name := "connected"
			if evtType == 0x02 {
				name = "disconnected"
			}
			select {
			case n.events <- BLEEvent{Type: name, Slot: int(data[1])}:
			default:
			}
		}
	}
}
