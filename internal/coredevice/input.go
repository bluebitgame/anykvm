package coredevice

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/danielpaulus/go-ios/ios/xpc"
)

const keyboardServiceID uint64 = 1025
const universalFeature = "com.apple.coredevice.feature.remote.universalhidservice"
const legacyUniversalFeature = "com.apple.coredevice.feature.remote.universalhid"

var keyboardDescriptor = []byte{5, 1, 9, 6, 0xa1, 1, 5, 7, 0x19, 0xe0, 0x29, 0xe7, 0x15, 0, 0x25, 1, 0x95, 8, 0x75, 1, 0x81, 2, 0x95, 1, 0x75, 8, 0x81, 1, 5, 7, 0x19, 0, 0x29, 0xff, 0x15, 0, 0x26, 0xff, 0, 0x95, 6, 0x75, 8, 0x81, 0, 5, 8, 0x19, 1, 0x29, 5, 0x15, 0, 0x25, 1, 0x95, 5, 0x75, 1, 0x91, 2, 0x95, 1, 0x75, 3, 0x91, 1, 0xc0}

func hidPayload(payload map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"featureIdentifier": universalFeature, "messageType": "Request", "payload": payload}
}

func (c *xpcChannel) hidRequest(payload map[string]interface{}) map[string]interface{} {
	m := hidPayload(payload)
	if c.hidFeature != "" {
		m["featureIdentifier"] = c.hidFeature
	}
	return m
}
func keyboardRegistration() map[string]interface{} {
	pair := map[string]interface{}{"DeviceUsage": int64(6), "DeviceUsagePage": int64(1)}
	storage := map[string]interface{}{
		"Manufacturer": map[string]interface{}{"string": "AnyKVM"}, "Product": map[string]interface{}{"string": "AnyKVM Keyboard"},
		"ProductID": map[string]interface{}{"int": int64(1)}, "VendorID": map[string]interface{}{"int": int64(0x5ac)},
		"PrimaryUsage": map[string]interface{}{"int": int64(6)}, "PrimaryUsagePage": map[string]interface{}{"int": int64(1)},
		"DeviceUsagePairs": map[string]interface{}{"array": []interface{}{map[string]interface{}{"dictionary": map[string]interface{}{"DeviceUsage": map[string]interface{}{"int": int64(6)}, "DeviceUsagePage": map[string]interface{}{"int": int64(1)}}}}},
		"Transport":        map[string]interface{}{"string": "USB"}, "ReportDescriptor": map[string]interface{}{"data": keyboardDescriptor},
		"UniversalControlVirtualService": map[string]interface{}{"bool": true}, "_ServiceID": map[string]interface{}{"uint": keyboardServiceID},
	}
	return hidPayload(map[string]interface{}{"createService": map[string]interface{}{"_0": map[string]interface{}{"DeviceUsagePairs": []interface{}{pair}, "PrimaryUsage": uint64(6), "PrimaryUsagePage": uint64(1), "Product": "AnyKVM Keyboard", "ProductID": int64(1), "VendorID": int64(0x5ac), "_CoreDevice_codablePropertyStorage": storage, "_ServiceID": keyboardServiceID}}})
}
func (c *xpcChannel) registerKeyboard() (uint64, error) {
	m := keyboardRegistration()
	if c.hidFeature != "" {
		m["featureIdentifier"] = c.hidFeature
	}
	if err := c.x.Send(m, xpc.HeartbeatRequestFlag); err != nil {
		return 0, err
	}
	m, err := c.x.ReceiveOnServerClientStream()
	if err != nil {
		return 0, err
	}
	if id := number(m["serviceID"]); id != 0 {
		return id, nil
	}
	return 0, fmt.Errorf("coredevice: keyboard registration rejected: %v", m)
}

// Verify the real touchscreen surface before advertising standalone input.
func (c *xpcChannel) requireTouchscreen() error {
	if err := c.x.Send(c.hidRequest(map[string]interface{}{"connectedServices": map[string]interface{}{}}), xpc.HeartbeatRequestFlag); err != nil {
		return err
	}
	m, err := c.x.ReceiveOnServerClientStream()
	if err != nil {
		return err
	}
	services, ok := m["connectedServices"].([]interface{})
	if !ok {
		return fmt.Errorf("coredevice: malformed connectedServices response: %v", m)
	}
	for _, value := range services {
		service, _ := value.(map[string]interface{})
		if number(service["_ServiceID"]) == 257 {
			return nil
		}
	}
	return fmt.Errorf("coredevice: mainTouchscreen (257) unavailable; enable Developer Mode and mount a compatible Developer Disk Image")
}

func (e *endpoint) openInput(ctx context.Context, verifyTouchscreen bool) (*inputState, error) {
	var failures []error
	for _, variant := range []struct{ service, feature string }{{universalService, universalFeature}, {legacyUniversalService, legacyUniversalFeature}} {
		if e.ports[variant.service] == 0 {
			continue
		}
		channel, err := e.connect(ctx, variant.service)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		channel.hidFeature = variant.feature
		if verifyTouchscreen {
			err = channel.requireTouchscreen()
		}
		var id uint64
		if err == nil {
			id, err = channel.registerKeyboard()
		}
		if err == nil {
			return &inputState{channel: channel, keyboardID: id, x: 16384, y: 16384}, nil
		}
		channel.Close()
		failures = append(failures, fmt.Errorf("%s: %w", variant.service, err))
	}
	return nil, fmt.Errorf("coredevice: Universal HID initialization: %w", errors.Join(failures...))
}
func putTimestamp(p []byte, ts uint64) {
	for i := 0; i < 6; i++ {
		p[i] = byte(ts >> (8 * i))
	}
}
func touchReport(down bool, x, y int, ts uint64) []byte {
	p := make([]byte, 58)
	p[0], p[1], p[2], p[3] = 9, 1, 5, 2
	if down {
		p[3] = 0xc2
	}
	binary.LittleEndian.PutUint16(p[4:], uint16(clamp(x, 0, 32767)*65535/32767))
	binary.LittleEndian.PutUint16(p[6:], uint16(clamp(y, 0, 32767)*65535/32767))
	p[40] = 2
	putTimestamp(p[44:], ts)
	return p
}
func keyboardReport(keys [30]byte, ts uint64) []byte {
	p := make([]byte, 39)
	p[0] = 1
	copy(p[1:], keys[:])
	putTimestamp(p[31:], ts)
	return p
}
func clamp(n, lo, hi int) int { return min(max(n, lo), hi) }

type inputState struct {
	mu         sync.Mutex
	channel    *xpcChannel
	keyboardID uint64
	keys       [30]byte
	x, y       int
	// Standalone HID receives upright AVFoundation video coordinates. Web
	// DisplayService clients undo presentation rotation before sending input;
	// GameStream uses the encoded picture's axes directly. Neither direct
	// consumer applies the standalone USB rotation here.
	orientation           int32
	viewWidth, viewHeight int
	rotation              int
	contact               bool
	closed                bool
}

func (i *inputState) send(id uint64, p []byte) error {
	if i.closed {
		return nil
	}
	i.channel.conn.SetWriteDeadline(time.Now().Add(time.Second))
	defer i.channel.conn.SetWriteDeadline(time.Time{})
	return i.channel.x.Send(i.channel.hidRequest(map[string]interface{}{"send": map[string]interface{}{"_0": p, "_1": id}}), xpc.HeartbeatRequestFlag)
}
func (i *inputState) stamp() uint64 { return reportTimestamp() }
func (i *inputState) touch(down bool) error {
	x, y := inverseTouchRotation(i.x, i.y, i.rotation)
	return i.send(257, touchReport(down, x, y, i.stamp()))
}

func inverseTouchRotation(x, y, rotation int) (int, int) {
	switch rotation {
	case 90:
		return y, 32767 - x
	case 180:
		return 32767 - x, 32767 - y
	case 270:
		return 32767 - y, x
	default:
		return x, y
	}
}

// Called with mu held. Keep the previous mapping until the video dimensions
// agree with the app orientation: these updates can arrive in either order.
func (i *inputState) updateDisplayRotation() {
	if i.closed || i.orientation < 1 || i.orientation > 4 || i.viewWidth <= 0 || i.viewHeight <= 0 {
		return
	}
	sideways := i.orientation == 3 || i.orientation == 4
	if i.viewWidth != i.viewHeight && sideways != (i.viewWidth > i.viewHeight) {
		return
	}
	rotation := interfaceRotation(i.orientation)
	if rotation == i.rotation {
		return
	}
	// End a drag in its original coordinate system before changing axes.
	if i.contact {
		_ = i.touch(false)
		i.contact = false
	}
	i.rotation = rotation
	log.Printf("coredevice: USB touch mapping rotation=%d source=%dx%d", rotation, i.viewWidth, i.viewHeight)
}
func (i *inputState) close() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return
	}
	// Release even if a failed write left our local contact state uncertain.
	_ = i.touch(false)
	i.keys = [30]byte{}
	_ = i.send(i.keyboardID, keyboardReport(i.keys, i.stamp()))
	i.closed = true
	i.channel.Close()
}
func (c *Client) withInput(f func(*inputState) error) error {
	c.mu.Lock()
	s := c.session
	c.mu.Unlock()
	if s == nil {
		return nil
	}
	i := s.input
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return nil
	}
	return f(i)
}
func (c *Client) KeyDown(k uint8) error    { return c.key(k, true) }
func (c *Client) KeyRelease(k uint8) error { return c.key(k, false) }
func (c *Client) key(k uint8, down bool) error {
	if k >= 240 {
		return fmt.Errorf("coredevice: keyboard usage %d is outside the report bitmap", k)
	}
	return c.withInput(func(i *inputState) error {
		if down {
			i.keys[k/8] |= 1 << (k % 8)
		} else {
			i.keys[k/8] &^= 1 << (k % 8)
		}
		return i.send(i.keyboardID, keyboardReport(i.keys, i.stamp()))
	})
}
func (c *Client) KeyUp() error {
	return c.withInput(func(i *inputState) error {
		i.keys = [30]byte{}
		return i.send(i.keyboardID, keyboardReport(i.keys, i.stamp()))
	})
}
func (c *Client) MouseMove(x, y int) error {
	return c.withInput(func(i *inputState) error {
		i.x, i.y = clamp(x, 0, 32767), clamp(y, 0, 32767)
		if i.contact {
			return i.touch(true)
		}
		return nil
	})
}
func (c *Client) MouseMoveRel(dx, dy int) error {
	return c.withInput(func(i *inputState) error {
		w, h := int(c.width.Load()), int(c.height.Load())
		if w < 1 || h < 1 {
			return nil
		}
		i.x = clamp(i.x+dx*32767/w, 0, 32767)
		i.y = clamp(i.y+dy*32767/h, 0, 32767)
		if i.contact {
			return i.touch(true)
		}
		return nil
	})
}
func (c *Client) MouseButton(button int, down bool) error {
	if button != 1 {
		return nil
	} // iPhone touch has no right/middle button.
	return c.withInput(func(i *inputState) error {
		if i.contact == down {
			return nil
		}
		if err := i.touch(down); err != nil {
			return err
		}
		i.contact = down
		return nil
	})
}
func (c *Client) MouseWheel(delta int) error {
	return c.withInput(func(i *inputState) error {
		if delta == 0 || i.contact {
			return nil
		}
		x, y := i.x, i.y
		defer func() { i.x, i.y = x, y }()
		i.x = clamp(i.x, 4096, 28671)
		i.y = clamp(i.y, 8192, 24575)
		start := i.y
		end := clamp(start+clamp(delta, -8, 8)*1500, 4096, 28671)
		if err := i.touch(true); err != nil {
			return err
		}
		defer func() { _ = i.touch(false) }()
		for step := 1; step <= 6; step++ {
			i.y = start + (end-start)*step/6
			if err := i.touch(true); err != nil {
				return err
			}
			time.Sleep(8 * time.Millisecond)
		}
		return nil
	})
}
