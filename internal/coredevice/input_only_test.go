package coredevice

import (
	"errors"
	"net"
	"testing"

	"anykvm/internal/capture"
)

type inputXPCFixture struct {
	messages []map[string]interface{}
	replies  []map[string]interface{}
}

func (x *inputXPCFixture) Send(m map[string]interface{}, _ ...uint32) error {
	x.messages = append(x.messages, m)
	return nil
}
func (x *inputXPCFixture) ReceiveOnClientServerStream() (map[string]interface{}, error) {
	return nil, errors.New("unused")
}
func (x *inputXPCFixture) ReceiveOnServerClientStream() (map[string]interface{}, error) {
	if len(x.replies) == 0 {
		return nil, errors.New("no reply")
	}
	r := x.replies[0]
	x.replies = x.replies[1:]
	return r, nil
}

func TestInputOnlyServicesDoNotRequireDisplay(t *testing.T) {
	for _, service := range []string{universalService, legacyUniversalService} {
		e := &endpoint{ports: map[string]int{service: 123}}
		if err := e.requireServices(false); err != nil {
			t.Fatal(err)
		}
		if err := e.requireServices(true); err == nil {
			t.Fatal("video accepted without DisplayService")
		}
	}
	if err := (&endpoint{ports: map[string]int{displayService: 123}}).requireServices(false); err == nil {
		t.Fatal("input accepted without HID")
	}
}

func TestStandaloneInputVariantsAndCleanup(t *testing.T) {
	for _, feature := range []string{universalFeature, legacyUniversalFeature} {
		t.Run(feature, func(t *testing.T) {
			a, b := net.Pipe()
			defer b.Close()
			x := &inputXPCFixture{replies: []map[string]interface{}{
				{"connectedServices": []interface{}{map[string]interface{}{"_ServiceID": uint64(257), "Product": "CoreDevice touchscreen(nil)"}}},
				{"serviceID": uint64(1025)},
			}}
			channel := &xpcChannel{x: x, conn: a, cancel: func() bool { return true }, hidFeature: feature}
			if err := channel.requireTouchscreen(); err != nil {
				t.Fatal(err)
			}
			id, err := channel.registerKeyboard()
			if err != nil || id != 1025 {
				t.Fatal(id, err)
			}
			c, err := NewInputClient("coredevice:test-phone", Options{})
			if err != nil {
				t.Fatal(err)
			}
			s := &session{input: &inputState{channel: channel, keyboardID: id}, endpoint: &endpoint{}, frames: make(chan *capture.Frame, 1), done: make(chan struct{}), workerDone: make(chan struct{})}
			close(s.workerDone)
			c.session = s
			if err := c.MouseMove(8192, 24575); err != nil {
				t.Fatal(err)
			}
			if err := c.MouseButton(1, true); err != nil {
				t.Fatal(err)
			}
			if err := c.KeyDown(4); err != nil {
				t.Fatal(err)
			}
			if err := c.RequestKeyframe(); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Capture(); !errors.Is(err, capture.ErrStreamStopped) {
				t.Fatal("input client exposed a video stream", err)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if len(x.messages) != 6 {
				t.Fatal("expected query, keyboard registration, touch, key, and two release messages", len(x.messages))
			}
			for _, m := range x.messages {
				if m["featureIdentifier"] != feature {
					t.Fatal("wrong HID feature", m)
				}
				if _, ok := m["CoreDevice.featureIdentifier"]; ok {
					t.Fatal("standalone HID invoked DisplayService")
				}
			}
		})
	}
}

func TestStandaloneInputRejectsMissingOrMalformedTouchscreen(t *testing.T) {
	for _, response := range []map[string]interface{}{{}, {"connectedServices": "invalid"}, {"connectedServices": []interface{}{map[string]interface{}{"_ServiceID": uint64(1025)}}}} {
		x := &inputXPCFixture{replies: []map[string]interface{}{response}}
		if err := (&xpcChannel{x: x}).requireTouchscreen(); err == nil {
			t.Fatal("missing touchscreen accepted", response)
		}
	}
}
