package bridge

import (
	"anykvm/internal/capture"
	"testing"
)

type remoteCursorDevice struct {
	blockingCapture
	state *capture.CursorState
}

func (d *remoteCursorDevice) CaptureCursor() (*capture.CursorState, error) { return d.state, nil }

type localCursorFixture struct{ calls int }

func (*localCursorFixture) Open() error  { return nil }
func (*localCursorFixture) Close() error { return nil }
func (c *localCursorFixture) Capture() (*capture.CursorState, error) {
	c.calls++
	return &capture.CursorState{HotX: 99}, nil
}

type nativeLocalCursorFixture struct {
	localCursorFixture
	nativeCalls int
}

func (c *nativeLocalCursorFixture) CaptureNative() (*capture.CursorState, error) {
	c.nativeCalls++
	return &capture.CursorState{HotX: 198, SourceWidth: 3840, SourceHeight: 2160}, nil
}

func TestNativeCursorFollowsSource(t *testing.T) {
	remote := &remoteCursorDevice{state: &capture.CursorState{HotX: 3}}
	b := NewBridge([]*MachineSession{{Capture: remote, CursorRendering: "client"}, {Capture: &blockingCapture{}, CursorRendering: "client"}}, "", 30, 2, 2)
	local := &nativeLocalCursorFixture{}
	cursor := b.NewCursorCapture(local).(capture.NativeCursorCapture)
	state, _ := cursor.CaptureNative()
	if state != remote.state || local.nativeCalls != 0 {
		t.Fatal("native path substituted host cursor for remote source")
	}
	remote.state = nil
	state, _ = cursor.CaptureNative()
	if state != nil || local.nativeCalls != 0 {
		t.Fatal("native path substituted host cursor for missing remote shape")
	}
	b.mu.Lock()
	b.active = 1
	b.mu.Unlock()
	state, _ = cursor.CaptureNative()
	if state.HotX != 198 || state.SourceWidth != 3840 || local.nativeCalls != 1 || local.calls != 0 {
		t.Fatal("local native path lost Retina geometry")
	}
	state, _ = cursor.(capture.CursorCapture).Capture()
	if state.HotX != 99 || local.calls != 1 {
		t.Fatal("logical capture behavior changed")
	}
}
func TestRemoteCursorFollowsSource(t *testing.T) {
	remote := &remoteCursorDevice{state: &capture.CursorState{HotX: 3}}
	b := NewBridge([]*MachineSession{{Capture: remote, CursorRendering: "client"}, {Capture: &blockingCapture{}, CursorRendering: "client"}}, "", 30, 2, 2)
	local := &localCursorFixture{}
	cursor := b.NewCursorCapture(local)
	state, _ := cursor.Capture()
	if state.HotX != 3 || local.calls != 0 {
		t.Fatal("remote source captured host cursor")
	}
	remote.state = nil
	if state, _ := cursor.Capture(); state != nil || local.calls != 0 {
		t.Fatal("missing remote cursor substituted host cursor")
	}
	b.mu.Lock()
	b.active = 1
	b.mu.Unlock()
	state, _ = cursor.Capture()
	if state.HotX != 99 || local.calls != 1 {
		t.Fatal("local source cursor not restored")
	}
	state, _ = cursor.(capture.NativeCursorCapture).CaptureNative()
	if state.HotX != 99 || local.calls != 2 {
		t.Fatal("native request broke a legacy capture provider")
	}
}

func TestCaptureCardCursorModesAndRestoresLocal(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "logical", true: "native"}[native], func(t *testing.T) {
			card := capture.NewDevice("Hagibis")
			b := NewBridge([]*MachineSession{{Capture: &blockingCapture{}, CursorRendering: "client"}, {Capture: card, CursorRendering: "client"}}, "", 30, 2, 2)
			local := &nativeLocalCursorFixture{}
			cursor := b.NewCursorCapture(local)
			read := cursor.Capture
			if native {
				read = cursor.(capture.NativeCursorCapture).CaptureNative
			}
			before, err := read()
			if err != nil || before == nil {
				t.Fatalf("local cursor: %v", err)
			}
			calls := local.calls + local.nativeCalls
			b.mu.Lock()
			b.active = 1
			b.mu.Unlock()
			for _, mode := range []string{"", "absolute", "relative", "absolute"} {
				b.mu.Lock()
				b.machines[1].MouseMode = mode
				b.mu.Unlock()
				state, err := read()
				if err != nil || state == nil || state.Width != 0 || state.Height != 0 || len(state.Image) != 0 || state.SystemDefault != (mode != "relative") {
					t.Fatalf("unexpected capture card cursor: %+v, %v", state, err)
				}
			}
			if local.calls+local.nativeCalls != calls {
				t.Fatal("capture card polled host cursor")
			}
			b.mu.Lock()
			b.active = 0
			b.mu.Unlock()
			after, err := read()
			if err != nil || after == nil || after.HotX != before.HotX || local.calls+local.nativeCalls != calls+1 {
				t.Fatalf("local cursor not restored: %+v, %v", after, err)
			}
		})
	}
}

func TestClientCursorVisibilityPolicy(t *testing.T) {
	for _, relative := range []bool{false, true} {
		for _, rendering := range []string{"", "remote", "client"} {
			for _, card := range []bool{false, true} {
				var dev capture.Device = &remoteCursorDevice{state: &capture.CursorState{HotX: 3}}
				if card {
					dev = capture.NewDevice("Hagibis")
				}
				mode := "absolute"
				if relative {
					mode = "relative"
				}
				b := NewBridge([]*MachineSession{{Capture: dev, MouseMode: mode, CursorRendering: rendering}}, "", 30, 2, 2)
				local := &nativeLocalCursorFixture{}
				cursor := b.NewCursorCapture(local)
				for _, read := range []func() (*capture.CursorState, error){cursor.Capture, cursor.(capture.NativeCursorCapture).CaptureNative} {
					state, err := read()
					if err != nil || state == nil {
						t.Fatalf("cursor: %+v %v", state, err)
					}
					hidden := relative || rendering == "remote"
					if hidden {
						if state.SystemDefault || state.HotX != 0 || len(state.Image) != 0 || state.Width != 0 || state.Height != 0 {
							t.Fatalf("relative=%v cursor=%q card=%v: expected hidden, got %+v", relative, rendering, card, state)
						}
					} else if state.SystemDefault != card || (!card && state.HotX != 3) {
						t.Fatalf("absolute client rendering: card=%v state=%+v", card, state)
					}
				}
				if local.calls+local.nativeCalls != 0 {
					t.Fatal("unexpected host cursor capture")
				}
			}
		}
	}
}
