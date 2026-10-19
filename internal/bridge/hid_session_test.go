package bridge

import "testing"

type sessionHID struct {
	HIDSender
	opened        bool
	starts, stops int
}

func (h *sessionHID) StartSession() error {
	if !h.opened {
		h.starts++
		h.opened = true
	}
	return nil
}
func (h *sessionHID) StopSession() error {
	if h.opened {
		h.stops++
		h.opened = false
	}
	return nil
}

func TestTargetHIDFollowsControlledTarget(t *testing.T) {
	first, second := &sessionHID{}, &sessionHID{}
	b := NewBridge([]*MachineSession{{HID: first}, {HID: second}}, "alt", 30, 0, 0)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	if first.opened || second.opened {
		t.Fatal("idle target opened HID")
	}
	b.ClaimVNC()
	b.OnClientConnected()
	if !first.opened || second.opened {
		t.Fatal("controlled target HID not opened")
	}
	b.SwitchMachine(1)
	if first.opened || !second.opened {
		t.Fatal("switch did not release old HID")
	}
	b.OnClientDisconnected()
	if first.opened || second.opened {
		t.Fatal("disconnect did not release HID")
	}
	b.OnClientConnected()
	if !second.opened || second.starts != 2 {
		t.Fatal("reconnect did not reopen HID")
	}
	b.Stop()
	if second.opened {
		t.Fatal("shutdown did not release HID")
	}
}
