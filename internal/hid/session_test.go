package hid

import (
	"errors"
	"testing"
)

type sessionBackend struct {
	hidi
	keys, releases, closes int
}

func (s *sessionBackend) KeyDown(uint8) error         { s.keys++; return nil }
func (s *sessionBackend) KeyUp() error                { s.releases++; return nil }
func (s *sessionBackend) MouseButton(int, bool) error { return nil }
func (s *sessionBackend) Close() error                { s.closes++; return nil }

func TestSessionDeviceReleasesPortAndReopens(t *testing.T) {
	backend := &sessionBackend{}
	opens := 0
	s := &sessionDevice{open: func() (*Device, error) { opens++; return &Device{impl: backend}, nil }}
	d := &Device{impl: s}
	_ = d.KeyDown(1)
	_ = d.StopSession()
	if opens != 0 || backend.keys != 0 {
		t.Fatal("idle input opened device")
	}
	for cycle := 1; cycle <= 2; cycle++ {
		if err := d.StartSession(); err != nil {
			t.Fatal(err)
		}
		if err := d.StartSession(); err != nil {
			t.Fatal(err)
		}
		_ = d.KeyDown(1)
		_ = d.StopSession()
		_ = d.Close()
		if opens != cycle || backend.keys != cycle || backend.releases != cycle || backend.closes != cycle {
			t.Fatalf("cycle %d: opens=%d backend=%+v", cycle, opens, backend)
		}
	}
}

func TestSessionDeviceRetriesFailedOpen(t *testing.T) {
	s := &sessionDevice{open: func() (*Device, error) { return nil, errors.New("busy") }}
	if err := s.StartSession(); err == nil {
		t.Fatal("missing open error")
	}
	s.open = func() (*Device, error) { return &Device{impl: &sessionBackend{}}, nil }
	if err := s.StartSession(); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
}
