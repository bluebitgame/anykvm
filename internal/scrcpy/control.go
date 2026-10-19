package scrcpy

import (
	"encoding/binary"
	"time"
)

// scrcpy control message types (scrcpy >= 3.0).
const (
	cmsgInjectKeycode = 0
	cmsgInjectTouch   = 2
	cmsgInjectScroll  = 3
)

// Android key/motion event actions.
const (
	aKeyActionDown = 0
	aKeyActionUp   = 1

	aMotionDown      = 0
	aMotionUp        = 1
	aMotionMove      = 2
	aMotionHoverMove = 7
)

// pointerIDMouse (-1) makes the server inject events with a mouse source.
const pointerIDMouse = ^uint64(0)

// hidButtonToAndroid maps AnyKVM HID button numbers (1=left, 2=right,
// 3=middle) to Android MotionEvent button masks (PRIMARY/SECONDARY/TERTIARY).
var hidButtonToAndroid = map[int]uint32{1: 1, 2: 2, 3: 4}

// Session implements the vnc/rdp/wsmedia HIDSender interface by translating
// USB HID semantics into scrcpy control messages. Input is silently dropped
// while the session is not connected (input without video is useless).

// KeyDown presses a key identified by its USB HID usage code.
func (s *Session) KeyDown(usage uint8) error {
	keycode, ok := hidToAndroidKey[usage]
	if !ok {
		return nil
	}
	s.inMu.Lock()
	s.pressed[usage] = true
	meta := s.metaStateLocked()
	s.inMu.Unlock()
	return s.sendKey(aKeyActionDown, keycode, meta)
}

// KeyRelease releases a key identified by its USB HID usage code.
func (s *Session) KeyRelease(usage uint8) error {
	keycode, ok := hidToAndroidKey[usage]
	if !ok {
		return nil
	}
	s.inMu.Lock()
	delete(s.pressed, usage)
	meta := s.metaStateLocked()
	s.inMu.Unlock()
	return s.sendKey(aKeyActionUp, keycode, meta)
}

// KeyUp releases all currently pressed keys.
func (s *Session) KeyUp() error {
	s.inMu.Lock()
	usages := make([]uint8, 0, len(s.pressed))
	for u := range s.pressed {
		usages = append(usages, u)
	}
	s.pressed = make(map[uint8]bool)
	s.inMu.Unlock()

	var err error
	for _, u := range usages {
		if keycode, ok := hidToAndroidKey[u]; ok {
			if e := s.sendKey(aKeyActionUp, keycode, 0); e != nil {
				err = e
			}
		}
	}
	return err
}

// MouseMove moves the pointer to an absolute position in the 0..32767 range
// (both axes), matching the semantics of the other HID backends.
func (s *Session) MouseMove(x, y int) error {
	s.inMu.Lock()
	s.lastX, s.lastY = clamp15(x), clamp15(y)
	buttons := s.buttons
	s.inMu.Unlock()

	action := byte(aMotionHoverMove)
	pressure := uint16(0)
	if buttons != 0 {
		action = aMotionMove
		pressure = 0xFFFF
	}
	return s.sendTouch(action, buttons, 0, pressure)
}

// MouseMoveRel applies a relative delta to the tracked absolute position and
// forwards it as an absolute motion event (scrcpy's protocol is absolute).
func (s *Session) MouseMoveRel(dx, dy int) error {
	s.inMu.Lock()
	s.lastX = clamp15(s.lastX + dx)
	s.lastY = clamp15(s.lastY + dy)
	buttons := s.buttons
	s.inMu.Unlock()

	action := byte(aMotionHoverMove)
	pressure := uint16(0)
	if buttons != 0 {
		action = aMotionMove
		pressure = 0xFFFF
	}
	return s.sendTouch(action, buttons, 0, pressure)
}

// MouseButton presses/releases a mouse button (1=left, 2=right, 3=middle).
func (s *Session) MouseButton(button int, down bool) error {
	abtn, ok := hidButtonToAndroid[button]
	if !ok {
		return nil
	}
	s.inMu.Lock()
	if down {
		s.buttons |= abtn
	} else {
		s.buttons &^= abtn
	}
	buttons := s.buttons
	s.inMu.Unlock()

	action := byte(aMotionUp)
	pressure := uint16(0)
	if down {
		action = aMotionDown
		pressure = 0xFFFF
	}
	return s.sendTouch(action, buttons, abtn, pressure)
}

// MouseWheel scrolls vertically by delta steps (positive = up).
func (s *Session) MouseWheel(delta int) error {
	w, h := s.Size()
	if w <= 0 || h <= 0 {
		return nil
	}
	s.inMu.Lock()
	x := s.lastX * (w - 1) / 32767
	y := s.lastY * (h - 1) / 32767
	buttons := s.buttons
	s.inMu.Unlock()

	// scrcpy 3.3.1 expanded the fixed-point range from [-1, 1] to [-16, 16].
	// Keep a wheel step equal to one Android scroll unit on either protocol.
	v := delta
	if v > 1 {
		v = 1
	}
	if v < -1 {
		v = -1
	}
	vscroll := int16(v * 32767)
	s.mu.Lock()
	if s.scroll16 {
		vscroll = int16(v * 2048)
	}
	s.mu.Unlock()

	buf := make([]byte, 21)
	buf[0] = cmsgInjectScroll
	binary.BigEndian.PutUint32(buf[1:], uint32(int32(x)))
	binary.BigEndian.PutUint32(buf[5:], uint32(int32(y)))
	binary.BigEndian.PutUint16(buf[9:], uint16(w))
	binary.BigEndian.PutUint16(buf[11:], uint16(h))
	binary.BigEndian.PutUint16(buf[13:], 0) // hscroll
	binary.BigEndian.PutUint16(buf[15:], uint16(vscroll))
	binary.BigEndian.PutUint32(buf[17:], buttons)
	return s.sendControl(buf)
}

// sendKey emits an inject-keycode control message.
func (s *Session) sendKey(action byte, keycode int32, metaState uint32) error {
	buf := make([]byte, 14)
	buf[0] = cmsgInjectKeycode
	buf[1] = action
	binary.BigEndian.PutUint32(buf[2:], uint32(keycode))
	binary.BigEndian.PutUint32(buf[6:], 0) // repeat
	binary.BigEndian.PutUint32(buf[10:], metaState)
	return s.sendControl(buf)
}

// sendTouch emits an inject-touch-event control message at the last pointer
// position. actionButton is the button whose state changed (0 for moves).
func (s *Session) sendTouch(action byte, buttons, actionButton uint32, pressure uint16) error {
	w, h := s.Size()
	if w <= 0 || h <= 0 {
		return nil
	}
	s.inMu.Lock()
	x := s.lastX * (w - 1) / 32767
	y := s.lastY * (h - 1) / 32767
	s.inMu.Unlock()

	buf := make([]byte, 32)
	buf[0] = cmsgInjectTouch
	buf[1] = action
	binary.BigEndian.PutUint64(buf[2:], pointerIDMouse)
	binary.BigEndian.PutUint32(buf[10:], uint32(int32(x)))
	binary.BigEndian.PutUint32(buf[14:], uint32(int32(y)))
	binary.BigEndian.PutUint16(buf[18:], uint16(w))
	binary.BigEndian.PutUint16(buf[20:], uint16(h))
	binary.BigEndian.PutUint16(buf[22:], pressure)
	binary.BigEndian.PutUint32(buf[24:], actionButton)
	binary.BigEndian.PutUint32(buf[28:], buttons)
	return s.sendControl(buf)
}

// sendControl writes one control message to the control socket. Messages are
// dropped without error while the session is not connected.
func (s *Session) sendControl(msg []byte) error {
	s.mu.Lock()
	conn := s.control
	running := s.running
	s.mu.Unlock()
	if !running || conn == nil {
		return nil
	}

	s.ctlMu.Lock()
	defer s.ctlMu.Unlock()
	conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, err := conn.Write(msg)
	return err
}

// metaStateLocked computes the Android meta state from the currently pressed
// modifier usages. Caller holds s.inMu.
func (s *Session) metaStateLocked() uint32 {
	var meta uint32
	for usage := range s.pressed {
		if m, ok := hidModifierMeta[usage]; ok {
			meta |= m
		}
	}
	return meta
}

func clamp15(v int) int {
	if v < 0 {
		return 0
	}
	if v > 32767 {
		return 32767
	}
	return v
}
