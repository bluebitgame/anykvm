package hid

import (
	"errors"
	"testing"
)

type textBackend struct {
	hidi
	text string
	err  error
}

func (h *textBackend) SendText(text string) error { h.text += text; return h.err }

func TestDeviceTextCapability(t *testing.T) {
	backend := &textBackend{}
	d := &Device{impl: backend}
	if err := d.SendText("中文😀"); err != nil || backend.text != "中文😀" {
		t.Fatal("text was not forwarded", err)
	}
	backend.err = errors.New("text failed")
	if err := d.SendText("a"); !errors.Is(err, backend.err) {
		t.Fatal("text error lost")
	}
	if err := (&Device{}).SendText("a"); !errors.Is(err, ErrTextUnsupported) {
		t.Fatal("unsupported backend did not signal fallback", err)
	}
}
