package hid

import (
	"bytes"
	"net"
	"testing"
	"time"
)

type testFile struct {
	rw      *bytes.Buffer
	onWrite func([]byte)
}

func (f *testFile) Read(p []byte) (int, error) { return f.rw.Read(p) }
func (f *testFile) Write(p []byte) (int, error) {
	n, err := f.rw.Write(p)
	if f.onWrite != nil {
		f.onWrite(p)
	}
	return n, err
}
func (f *testFile) Close() error { return nil }
func (f *testFile) SetReadDeadline(_ time.Time) error {
	return &net.OpError{Err: &timeoutError{}}
}

func (f *testFile) SetWriteDeadline(_ time.Time) error { return nil }

type timeoutError struct{}

func (e *timeoutError) Error() string   { return "i/o timeout" }
func (e *timeoutError) Timeout() bool   { return true }
func (e *timeoutError) Temporary() bool { return true }

func newTestNRFDevice(buf *bytes.Buffer) *serialHIDDevice {
	// These tests drive replies directly; no concurrent reader should consume
	// the same buffer that records outgoing commands.
	n := &serialHIDDevice{done: make(chan struct{}), events: make(chan BLEEvent, 16), last: ProfileStatus{Version: 1, Capabilities: 0x1f, SlotCount: 4}}
	n.rw = &testFile{rw: buf, onWrite: func(p []byte) { n.dispatchReply(0x84, [4]byte{p[1], 0}) }}
	return n
}

func TestNRFDeviceSendCmd(t *testing.T) {
	var buf bytes.Buffer
	n := newTestNRFDevice(&buf)

	err := n.sendCmd(0x22, [4]byte{})
	if err != nil {
		t.Fatal(err)
	}

	out := buf.Bytes()
	if len(out) != 8 {
		t.Fatalf("expected 8 bytes, got %d", len(out))
	}
	if out[0] != 0x33 {
		t.Errorf("header: got 0x%02x, want 0x33", out[0])
	}
	if out[1] != 0x22 {
		t.Errorf("cmd: got 0x%02x, want 0x22", out[1])
	}

	expectCRC := crc16modbus(out[:6])
	gotCRC := uint16(out[6])<<8 | uint16(out[7])
	if expectCRC != gotCRC {
		t.Errorf("CRC: got 0x%04x, want 0x%04x", gotCRC, expectCRC)
	}
}

func TestNRFDeviceKeyDown(t *testing.T) {
	var buf bytes.Buffer
	n := newTestNRFDevice(&buf)

	_ = n.KeyDown(0xE2)
	out := buf.Bytes()
	if out[1] != 0x11 {
		t.Errorf("cmd: got 0x%02x, want 0x11", out[1])
	}
	if out[2] != 0xE2 {
		t.Errorf("keycode: got 0x%02x, want 0xE2", out[2])
	}
	if out[3] != 1 {
		t.Errorf("state: got %d, want 1", out[3])
	}
}

func TestNRFDeviceMouseMove(t *testing.T) {
	var buf bytes.Buffer
	n := newTestNRFDevice(&buf)

	_ = n.MouseMove(16384, 32767)
	out := buf.Bytes()
	if out[1] != 0x12 {
		t.Errorf("cmd: got 0x%02x, want 0x12", out[1])
	}
	x := uint16(out[2])<<8 | uint16(out[3])
	if x != 16384 {
		t.Errorf("x: got %d, want 16384", x)
	}
	y := uint16(out[4])<<8 | uint16(out[5])
	if y != 32767 {
		t.Errorf("y: got %d, want 32767", y)
	}
}

func TestNRFDeviceMouseMoveRel(t *testing.T) {
	var buf bytes.Buffer
	n := newTestNRFDevice(&buf)

	_ = n.MouseMoveRel(200, -100)
	out := buf.Bytes()
	if len(out) < 16 {
		t.Fatalf("expected at least 16 bytes (2 frames), got %d", len(out))
	}
	if out[1] != 0x15 {
		t.Errorf("cmd: got 0x%02x, want 0x15", out[1])
	}
}

func TestNRFDeviceMouseButton(t *testing.T) {
	var buf bytes.Buffer
	n := newTestNRFDevice(&buf)

	_ = n.MouseButton(1, true)
	out := buf.Bytes()
	if out[1] != 0x13 {
		t.Errorf("cmd: got 0x%02x, want 0x13", out[1])
	}
	if out[2] != (0x80 | 0x08) {
		t.Errorf("button code: got 0x%02x, want 0x88", out[2])
	}

	buf.Reset()
	_ = n.MouseButton(1, false)
	out = buf.Bytes()
	if out[2] != 0x80 {
		t.Errorf("release code: got 0x%02x, want 0x80", out[2])
	}
}

func TestNRFDeviceMouseWheel(t *testing.T) {
	var buf bytes.Buffer
	n := newTestNRFDevice(&buf)

	_ = n.MouseWheel(-5)
	out := buf.Bytes()
	if out[1] != 0x14 {
		t.Errorf("cmd: got 0x%02x, want 0x14", out[1])
	}
	if int8(out[3]) != -5 {
		t.Errorf("wheel: got %d, want -5", int8(out[3]))
	}
}

func TestNRFDeviceProfileSelect(t *testing.T) {
	var buf bytes.Buffer
	n := newTestNRFDevice(&buf)

	_ = n.ProfileSelect(2)
	out := buf.Bytes()
	if out[1] != 0x20 {
		t.Errorf("cmd: got 0x%02x, want 0x20", out[1])
	}
	if out[2] != 2 {
		t.Errorf("slot: got %d, want 2", out[2])
	}
}

func TestNRFConfigureSessionSkipsRedundantMode(t *testing.T) {
	var buf bytes.Buffer
	n := newTestNRFDevice(&buf)

	// Status reply: slot 0, absolute mouse (0x20 flag).
	statusFrame := make([]byte, 8)
	statusFrame[0] = 0x33
	statusFrame[1] = 0x81
	statusFrame[2] = 0x20 // absolute
	statusFrame[3] = 0
	statusFrame[4] = 0
	statusFrame[5] = 0
	crc := crc16modbus(statusFrame[:6])
	statusFrame[6] = byte((crc >> 8) & 0xFF)
	statusFrame[7] = byte(crc & 0xFF)

	go func() {
		time.Sleep(20 * time.Millisecond)
		for _, b := range statusFrame {
			n.replyRxByte(b)
		}
	}()

	if err := configureSerialSession(n, true, SlotUnset); err != nil {
		t.Fatal(err)
	}
	out := buf.Bytes()
	for i := 0; i+1 < len(out); i += 8 {
		if out[i+1] == 0x04 {
			t.Fatal("SetMouseMode (0x04) sent when dongle already absolute")
		}
	}
	_ = n.Close()
}

func TestNRFDeviceSetMouseMode(t *testing.T) {
	var buf bytes.Buffer
	n := newTestNRFDevice(&buf)

	_ = n.SetMouseMode(true)
	out := buf.Bytes()
	if out[1] != 0x04 {
		t.Errorf("cmd: got 0x%02x, want 0x04", out[1])
	}
	if out[2] != 0x08 {
		t.Errorf("mode: got 0x%02x, want 0x08 (abs)", out[2])
	}
}

func TestReplyParseReady(t *testing.T) {
	var buf bytes.Buffer
	n := newTestNRFDevice(&buf)

	n.replyRxByte(0)
	frame := make([]byte, 8)
	frame[0] = 0x33
	frame[1] = 0x80
	frame[2] = 4
	frame[3] = 0
	frame[4] = 0
	frame[5] = 0
	crc := crc16modbus(frame[:6])
	frame[6] = byte((crc >> 8) & 0xFF)
	frame[7] = byte(crc & 0xFF)

	for _, b := range frame {
		n.replyRxByte(b)
	}
	if !n.last.Ready {
		t.Error("expected Ready=true")
	}
	if n.last.SlotCount != 4 {
		t.Errorf("SlotCount: got %d, want 4", n.last.SlotCount)
	}
	_ = n.Close()
}

func TestReplyParseStatus(t *testing.T) {
	var buf bytes.Buffer
	n := newTestNRFDevice(&buf)

	frame := make([]byte, 8)
	frame[0] = 0x33
	frame[1] = 0x81
	frame[2] = 1 | 0x04 | 0x08 | 0x20
	frame[3] = 0
	frame[4] = 0
	frame[5] = 0
	crc := crc16modbus(frame[:6])
	frame[6] = byte((crc >> 8) & 0xFF)
	frame[7] = byte(crc & 0xFF)

	for _, b := range frame {
		n.replyRxByte(b)
	}
	if n.last.Slot != 1 {
		t.Errorf("Slot: got %d, want 1", n.last.Slot)
	}
	if !n.last.Connected {
		t.Error("expected Connected=true")
	}
	if !n.last.Bonded {
		t.Error("expected Bonded=true")
	}
	if n.last.MouseMode != "absolute" {
		t.Errorf("MouseMode: got %q, want absolute", n.last.MouseMode)
	}
	_ = n.Close()
}

func TestReplyParseEvent(t *testing.T) {
	var buf bytes.Buffer
	n := newTestNRFDevice(&buf)

	frame := make([]byte, 8)
	frame[0] = 0x33
	frame[1] = 0x82
	frame[2] = 0x02 // disconnected
	frame[3] = 1    // slot 1
	frame[4] = 0
	frame[5] = 0
	crc := crc16modbus(frame[:6])
	frame[6] = byte((crc >> 8) & 0xFF)
	frame[7] = byte(crc & 0xFF)

	for _, b := range frame {
		n.replyRxByte(b)
	}

	select {
	case evt := <-n.events:
		if evt.Type != "disconnected" {
			t.Errorf("event type: got %q, want disconnected", evt.Type)
		}
		if evt.Slot != 1 {
			t.Errorf("event slot: got %d, want 1", evt.Slot)
		}
	case <-time.After(time.Second):
		t.Error("event not received")
	}
	_ = n.Close()
}

func TestProfileStatusTimeout(t *testing.T) {
	var buf bytes.Buffer
	n := newTestNRFDevice(&buf)

	done := make(chan error, 1)
	go func() {
		_, err := n.ProfileStatus()
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("expected timeout error")
		}
	case <-time.After(2 * time.Second):
		t.Error("ProfileStatus blocked indefinitely")
	}
	_ = n.Close()
}

func TestCancelWaiterAfterEarlierReply(t *testing.T) {
	first, second := make(chan ProfileStatus, 1), make(chan ProfileStatus, 1)
	n := &serialHIDDevice{stWait: []chan ProfileStatus{first, second}}
	n.dispatchReply(0x81, [4]byte{}) // consumes the first waiter, shifting the second
	if !n.cancelWaiter(second) || len(n.stWait) != 0 {
		t.Fatal("shifted waiter was not removed")
	}
}

func TestBLESingleSlotAdapter(t *testing.T) {
	var buf bytes.Buffer
	n := newTestNRFDevice(&buf)
	n.dispatchReply(0x80, [4]byte{1}) // ESP32-S3 advertises one profile.
	for _, action := range []func(int) error{n.ProfileSelect, n.ProfileClear} {
		for _, slot := range []int{-1, 1, 3, 255} {
			if err := action(slot); err == nil {
				t.Fatalf("accepted unsupported slot %d", slot)
			}
		}
		if buf.Len() != 0 {
			t.Fatal("invalid slot wrote a command")
		}
		if err := action(0); err != nil {
			t.Fatal(err)
		}
		buf.Reset()
	}
	n.dispatchReply(0x81, [4]byte{0x04 | 0x08 | 0x20})
	st := n.lastStatus()
	if st.SlotCount != 1 || !st.Ready || !st.Connected || !st.Bonded || st.MouseMode != "absolute" {
		t.Fatalf("single-slot status: %+v", st)
	}
}

func TestStatusQueryUnblocksOnClose(t *testing.T) {
	host, device := net.Pipe()
	n := newSerialDeviceRW(host)
	result := make(chan error, 1)
	go func() { _, err := n.ProfileStatus(); result <- err }()
	// Consume the status request, then close before any reply arrives.
	var request [8]byte
	if _, err := device.Read(request[:]); err != nil {
		t.Fatal(err)
	}
	go device.Close()
	n.Close()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("expected closed-device error")
		}
	case <-time.After(time.Second):
		t.Fatal("status query did not unblock on close")
	}
}
