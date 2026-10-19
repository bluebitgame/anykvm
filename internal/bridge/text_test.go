package bridge

import (
	"errors"
	"testing"

	"anykvm/internal/hid"
)

type textHID struct {
	HIDSender
	text string
	err  error
}

func (h *textHID) SendText(text string) error { h.text += text; return h.err }

func TestTextRoutesToActiveMachine(t *testing.T) {
	first, second := &textHID{}, &textHID{}
	b := NewBridge([]*MachineSession{{HID: first}, {HID: second}, {HID: struct{ HIDSender }{}}}, "alt", 30, 0, 0)
	if err := b.SendText("中"); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.active = 1
	b.mu.Unlock()
	if err := b.SendText("文😀"); err != nil {
		t.Fatal(err)
	}
	if first.text != "中" || second.text != "文😀" {
		t.Fatal("text routed to wrong machine")
	}
	second.err = errors.New("input failed")
	if err := b.SendText("a"); !errors.Is(err, second.err) {
		t.Fatal("backend error lost")
	}
	b.mu.Lock()
	b.active = 2
	b.mu.Unlock()
	if err := b.SendText("a"); !errors.Is(err, hid.ErrTextUnsupported) {
		t.Fatal("missing HID fallback signal", err)
	}
}
