package awdl

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

type interfaceEvent struct {
	operation string
	up        bool
	err       error
}

type fakeInterface struct {
	mu             sync.Mutex
	up             bool
	readErr        error
	downErr, upErr error
	changes        []bool
	events         chan interfaceEvent
}

func (f *fakeInterface) IsUp() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emit(interfaceEvent{"read", f.up, f.readErr})
	return f.up, f.readErr
}

func (f *fakeInterface) SetUp(up bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changes = append(f.changes, up)
	err := f.downErr
	if up {
		err = f.upErr
	}
	if err == nil {
		f.up = up
	}
	f.emit(interfaceEvent{"set", up, err})
	return err
}

func (f *fakeInterface) emit(event interfaceEvent) {
	select {
	case f.events <- event:
	default:
	}
}

func (f *fakeInterface) wait(t *testing.T, operation string, up bool) interfaceEvent {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-f.events:
			if event.operation == operation && event.up == up {
				return event
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for %s(up=%t)", operation, up)
		}
	}
}

func TestGuardDisablesReactivatedInterfaceAndRestores(t *testing.T) {
	f := &fakeInterface{up: true, events: make(chan interfaceEvent, 32)}
	g := newGuard(f, 10*time.Millisecond)
	defer g.Close()
	// Starting the server alone must not query or change the interface.
	f.mu.Lock()
	if len(f.changes) != 0 {
		t.Fatal("idle guard changed the interface")
	}
	f.mu.Unlock()
	g.SetActive(true)
	f.wait(t, "set", false)
	g.SetActive(true) // Additional clients must not take another state snapshot.
	f.mu.Lock()
	f.up = true // macOS re-enables AWDL during the session.
	f.mu.Unlock()
	f.wait(t, "set", false)
	g.SetActive(false)
	f.wait(t, "set", true)
	g.Close()
	if !reflect.DeepEqual(f.changes, []bool{false, false, true}) {
		t.Fatalf("interface transitions: %v", f.changes)
	}
}

func TestGuardPreservesOriginallyDisabledInterface(t *testing.T) {
	f := &fakeInterface{events: make(chan interfaceEvent, 32)}
	g := newGuard(f, 10*time.Millisecond)
	defer g.Close()
	g.SetActive(true)
	f.wait(t, "read", false)
	f.mu.Lock()
	f.up = true
	f.mu.Unlock()
	f.wait(t, "set", false)
	g.SetActive(false)
	g.Close()
	if !reflect.DeepEqual(f.changes, []bool{false}) {
		t.Fatalf("an originally disabled interface must not be enabled: %v", f.changes)
	}
}

func TestGuardCloseRestoresActiveSession(t *testing.T) {
	f := &fakeInterface{up: true, events: make(chan interfaceEvent, 32)}
	g := newGuard(f, time.Hour)
	g.SetActive(true)
	f.wait(t, "set", false)
	g.Close()
	g.SetActive(true) // Late disconnect/connect callbacks cannot restart a closed guard.
	g.Close()
	if !reflect.DeepEqual(f.changes, []bool{false, true}) {
		t.Fatalf("shutdown transitions: %v", f.changes)
	}
}

func TestGuardPermissionFailureDoesNotRestoreUnchangedInterface(t *testing.T) {
	f := &fakeInterface{up: true, downErr: errors.New("password required"), events: make(chan interfaceEvent, 32)}
	g := newGuard(f, time.Hour)
	g.SetActive(true)
	if event := f.wait(t, "set", false); event.err == nil {
		t.Fatal("expected permission failure")
	}
	g.SetActive(false)
	g.Close()
	if !reflect.DeepEqual(f.changes, []bool{false}) || !f.up {
		t.Fatalf("permission failure changed the original interface: %v, up=%t", f.changes, f.up)
	}
}

func TestGuardRetriesRestoration(t *testing.T) {
	f := &fakeInterface{up: true, upErr: errors.New("temporary failure"), events: make(chan interfaceEvent, 32)}
	g := newGuard(f, 20*time.Millisecond)
	defer g.Close()
	g.SetActive(true)
	f.wait(t, "set", false)
	g.SetActive(false)
	if event := f.wait(t, "set", true); event.err == nil {
		t.Fatal("expected first restoration to fail")
	}
	f.mu.Lock()
	f.upErr = nil
	f.mu.Unlock()
	if event := f.wait(t, "set", true); event.err != nil {
		t.Fatalf("restoration was not retried: %v", event.err)
	}
	g.Close()
	if !f.up {
		t.Fatal("interface was not restored")
	}
}

func TestGuardMissingInterfaceIsNonfatal(t *testing.T) {
	f := &fakeInterface{readErr: errors.New("no such interface"), events: make(chan interfaceEvent, 32)}
	g := newGuard(f, time.Hour)
	g.SetActive(true)
	f.wait(t, "read", false)
	g.Close()
	if len(f.changes) != 0 {
		t.Fatalf("missing interface triggered commands: %v", f.changes)
	}
	var disabled *Guard
	disabled.SetActive(true)
	disabled.Close()
}
