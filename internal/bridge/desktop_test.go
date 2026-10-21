package bridge

import (
	"anykvm/internal/capture"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type desktopCapture struct {
	directCapture
	requests []capture.DesktopSize
	changed  bool
	restores int
	failOpen bool
	selected capture.DesktopSize
	failSet  bool
}

type outputDesktopCapture struct {
	desktopCapture
	outputW, outputH       int
	preferredW, preferredH int
	downscaleOnly          bool
	maxW, maxH             int
}

func (d *outputDesktopCapture) SetOutputSize(w, h int)        { d.outputW, d.outputH = w, h }
func (d *outputDesktopCapture) SetOutputDownscaleOnly(v bool) { d.downscaleOnly = v }
func (d *outputDesktopCapture) SetOutputSizeLimit(w, h int)   { d.maxW, d.maxH = w, h }
func (d *outputDesktopCapture) OpenWithFormat(w, h, fps int) error {
	d.preferredW, d.preferredH = w, h
	if d.outputW > 0 && d.outputH > 0 {
		w, h = d.outputW, d.outputH
	}
	return d.desktopCapture.OpenWithFormat(w, h, fps)
}

type bitrateDesktopCapture struct {
	outputDesktopCapture
	bitrate, openedBitrate             int
	encoderFrames, openedEncoderFrames int
	auto, openedAuto                   bool
}

func (d *bitrateDesktopCapture) SetEncoderBufferFrames(frames int) { d.encoderFrames = frames }
func (d *bitrateDesktopCapture) SetVideoBitrate(kbps int)          { d.bitrate = kbps }
func (d *bitrateDesktopCapture) SetVideoBitrateAuto(auto bool) {
	d.auto = auto
}
func (d *bitrateDesktopCapture) OpenWithFormat(w, h, fps int) error {
	d.openedBitrate = d.bitrate
	d.openedEncoderFrames = d.encoderFrames
	d.openedAuto = d.auto
	return d.outputDesktopCapture.OpenWithFormat(w, h, fps)
}

func TestRDPNativeBitrateAcrossCaptureRestartsAndProtocols(t *testing.T) {
	dev := &bitrateDesktopCapture{}
	b := NewBridge([]*MachineSession{{Capture: dev, CapW: 1280, CapH: 720, FPS: 30}}, "alt", 0, 0, 0)
	b.SetRDPBitrate(20000)
	b.SetRDPEncoderBufferFrames(3)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	b.StartRDPWithSize("h264", 1280, 720)
	dev.waitForRead(t)
	if dev.openedEncoderFrames != 3 || dev.openedBitrate != 20000 || dev.openFPS != 30 || dev.preferredW != 1280 || dev.preferredH != 720 {
		t.Fatalf("native capture bitrate=%d fps=%d source=%dx%d", dev.openedBitrate, dev.openFPS, dev.preferredW, dev.preferredH)
	}
	if err := b.ResizeRDPDesktop(1920, 1080); err != nil {
		t.Fatal(err)
	}
	dev.waitForRead(t)
	if dev.openedEncoderFrames != 3 || dev.openedBitrate != 20000 || dev.openW != 1280 || dev.openH != 720 || dev.openFPS != 30 {
		t.Fatal("desktop resize lost bitrate/frame rate or exceeded Target size")
	}
	b.RequestRDPKeyframe() // Reopen when the backend cannot force a keyframe.
	dev.waitForRead(t)
	if dev.openedEncoderFrames != 3 || dev.openedBitrate != 20000 {
		t.Fatal("keyframe restart lost the RDP bitrate")
	}
	b.OnClientDisconnected()
	b.StartRDPWithCodec("planar")
	dev.waitForRead(t)
	if dev.openedEncoderFrames != 0 || dev.openedBitrate != 0 {
		t.Fatal("bitmap capture retained the native RDP encoding bitrate")
	}
	b.OnClientDisconnected()
	profile := capture.VideoStreamOptions{Codec: "h264", Width: 640, Height: 480, FPS: 60, Bitrate: 3000}
	if err := b.StartGameStream(0, profile); err != nil {
		t.Fatal(err)
	}
	dev.waitForRead(t)
	if dev.openedEncoderFrames != 0 || dev.openedBitrate != 0 || dev.profile == nil || *dev.profile != profile {
		t.Fatal("RDP bitrate leaked into GameStream or displaced its negotiated profile")
	}
}

func TestRDPAutoBitrateLeavesNativeRateControlUnset(t *testing.T) {
	dev := &bitrateDesktopCapture{}
	b := NewBridge([]*MachineSession{{Capture: dev, CapW: 1280, CapH: 720, FPS: 30}}, "alt", 0, 0, 0)
	b.SetRDPBitrate(0)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	b.StartRDPWithSize("h264", 1280, 720)
	dev.waitForRead(t)
	if dev.openedBitrate != 0 || !dev.openedAuto {
		t.Fatalf("native capture bitrate=%d auto=%t, want encoder-managed", dev.openedBitrate, dev.openedAuto)
	}
	b.OnClientDisconnected()
	b.StartRDPWithCodec("planar")
	dev.waitForRead(t)
	if dev.openedAuto {
		t.Fatal("bitmap capture retained native auto bitrate mode")
	}
}

func TestRDPNativeOutputScalingWithoutDesktopAuto(t *testing.T) {
	for _, native := range []bool{false, true} {
		dev := &outputDesktopCapture{}
		m := &MachineSession{Capture: dev, CapW: 1920, CapH: 1080}
		if native {
			m.CapW, m.CapH = 0, 0
		}
		b := NewBridge([]*MachineSession{m}, "alt", 30, 0, 0)
		if err := b.Start(); err != nil {
			t.Fatal(err)
		}
		b.StartRDPWithSize("clearcodec", 1976, 952)
		dev.waitForRead(t)
		wantW, wantH := 1976, 952
		if !native {
			wantW, wantH = 1920, 1080
		}
		if !dev.downscaleOnly || dev.openW != wantW || dev.openH != wantH || dev.preferredW != m.CapW || dev.preferredH != m.CapH || dev.maxW != m.CapW || dev.maxH != m.CapH {
			t.Fatalf("capture preferred=%dx%d output=%dx%d", dev.preferredW, dev.preferredH, dev.openW, dev.openH)
		}
		if err := b.ResizeRDPDesktop(1280, 720); err != nil {
			t.Fatal(err)
		}
		dev.waitForRead(t)
		if dev.openW != 1280 || dev.openH != 720 || len(dev.requests) != 0 {
			t.Fatal("RDP output resize changed the physical desktop or did not scale capture")
		}
		b.OnClientDisconnected()
		b.StartGameStream(0, capture.VideoStreamOptions{Width: 640, Height: 480, FPS: 30, Bitrate: 1000})
		dev.waitForRead(t)
		if dev.downscaleOnly || dev.outputW != 0 || dev.outputH != 0 || dev.maxW != 0 || dev.maxH != 0 {
			t.Fatal("RDP output override survived protocol takeover")
		}
		b.Stop()
	}
}

func TestRDPTargetCeilingRecomputedOnSwitchWithoutChangingClientRequest(t *testing.T) {
	first, second := &outputDesktopCapture{}, &outputDesktopCapture{}
	b := NewBridge([]*MachineSession{
		{Capture: first, CapW: 1440, CapH: 900},
		{Capture: second, CapW: 1920, CapH: 1080},
	}, "alt", 30, 0, 0)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	b.StartRDPWithSize("h264", 2560, 1440)
	first.waitForRead(t)
	if first.openW != 1440 || first.openH != 900 {
		t.Fatal("client request overrode first Target")
	}
	b.SwitchMachine(1)
	second.waitForRead(t)
	if second.openW != 1920 || second.openH != 1080 {
		t.Fatal("previous Target cap replaced the original client request")
	}
	if err := b.ResizeRDPDesktop(1280, 720); err != nil {
		t.Fatal(err)
	}
	second.waitForRead(t)
	if second.openW != 1280 || second.openH != 720 {
		t.Fatal("ceiling prevented downscaling")
	}
	if err := b.ResizeRDPDesktop(3840, 2160); err != nil {
		t.Fatal(err)
	}
	second.waitForRead(t)
	if second.openW != 1920 || second.openH != 1080 {
		t.Fatal("resize bypassed Target ceiling")
	}
}

type nativeResizeCapture struct{ outputDesktopCapture }

func (d *nativeResizeCapture) Capture() (*capture.Frame, error) {
	d.mu.Lock()
	d.reads++
	first, stop := d.reads == 1, d.stop
	w, h := d.openW, d.openH
	if first {
		close(d.entered)
	}
	d.mu.Unlock()
	if first {
		return &capture.Frame{Format: capture.FormatH264, Width: w, Height: (h + 1) &^ 1, Data: []byte{0, 0, 1, 0x65, 0x80}}, nil
	}
	<-stop
	return nil, errors.New("closed")
}

type nativeResizeSink struct{ frames chan Frame }

func (s *nativeResizeSink) PushFrame(f Frame)            { s.frames <- f }
func (*nativeResizeSink) UpdateFramebufferSize(int, int) {}
func (*nativeResizeSink) CloseClients()                  {}

func TestRDPH264CaptureProducesRequestedOutput(t *testing.T) {
	dev := &nativeResizeCapture{}
	b := NewBridge([]*MachineSession{{Capture: dev, CapW: 1920, CapH: 1080}}, "alt", 30, 0, 0)
	sink := &nativeResizeSink{frames: make(chan Frame, 8)}
	b.SetRDP(sink)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	b.StartRDPWithSize("h264", 800, 600)
	for _, size := range [][2]int{{800, 600}, {1024, 769}, {640, 480}} {
		if err := b.ResizeRDPDesktop(size[0], size[1]); err != nil {
			t.Fatal(err)
		}
		select {
		case f := <-sink.frames:
			if !f.CaptureScalesOutput || f.Format != capture.FormatH264 || f.Width != size[0] || f.Height != (size[1]+1)&^1 || f.OutputMaxW != 1920 || f.OutputMaxH != 1080 {
				t.Fatalf("requested=%v captured=%+v", size, f)
			}
		case <-time.After(time.Second):
			t.Fatal("native resized capture did not arrive")
		}
		if dev.preferredW != 1920 || dev.preferredH != 1080 || len(dev.requests) != 0 {
			t.Fatal("output resize changed the preferred source size or physical desktop")
		}
	}
}

type slowDesktopCapture struct {
	desktopCapture
	block             atomic.Bool
	switching, resume chan struct{}
}

func (d *slowDesktopCapture) SetDesktopSize(size capture.DesktopSize) (capture.DesktopSize, error) {
	if d.block.Swap(false) {
		close(d.switching)
		<-d.resume
	}
	return d.desktopCapture.SetDesktopSize(size)
}

type desktopInputHID struct {
	HIDSender
	keys chan uint8
}

type slowRestoreCapture struct {
	desktopCapture
	block           atomic.Bool
	entered, resume chan struct{}
}

func (d *slowRestoreCapture) RestoreDesktopSize() error {
	if d.block.Swap(false) {
		close(d.entered)
		<-d.resume
	}
	return d.desktopCapture.RestoreDesktopSize()
}

func TestRDPReconnectDuringDesktopRestoreKeepsOwnership(t *testing.T) {
	dev := &slowRestoreCapture{entered: make(chan struct{}), resume: make(chan struct{})}
	b := NewBridge([]*MachineSession{{Capture: dev, AutoResolution: true}}, "alt", 30, 0, 0)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	b.StartRDPWithSize("clearcodec", 1976, 952)
	dev.waitForRead(t)
	var once sync.Once
	unblock := func() { once.Do(func() { close(dev.resume) }) }
	defer func() { unblock(); b.Stop() }()
	dev.block.Store(true)
	oldDone := make(chan struct{})
	go func() { b.OnClientDisconnected(); close(oldDone) }()
	select {
	case <-dev.entered:
	case <-time.After(time.Second):
		t.Fatal("old desktop restoration did not begin")
	}
	newDone := make(chan struct{})
	go func() { b.StartRDPWithSize("clearcodec", 1976, 952); close(newDone) }()
	deadline := time.Now().Add(time.Second)
	reserved := false
	for time.Now().Before(deadline) {
		b.mu.Lock()
		reserved = b.consumers == 1
		b.mu.Unlock()
		if reserved {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !reserved {
		t.Fatal("new session did not reserve capture")
	}
	unblock()
	<-oldDone
	<-newDone
	dev.waitForRead(t)
	if err := b.ResizeRDPDesktop(1600, 900); err != nil {
		t.Fatalf("old cleanup cleared the new RDP owner's state: %v", err)
	}
	dev.waitForRead(t)
	b.OnClientDisconnected()
}

func (h desktopInputHID) KeyDown(key uint8) error { h.keys <- key; return nil }

func TestDesktopSwitchDoesNotBlockHID(t *testing.T) {
	dev := &slowDesktopCapture{switching: make(chan struct{}), resume: make(chan struct{})}
	keys := make(chan uint8, 1)
	b := NewBridge([]*MachineSession{{Capture: dev, AutoResolution: true, HID: desktopInputHID{keys: keys}}}, "alt", 30, 0, 0)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	b.StartRDPWithSize("clearcodec", 1976, 952)
	dev.waitForRead(t)
	var once sync.Once
	unblock := func() { once.Do(func() { close(dev.resume) }) }
	defer func() { unblock(); b.Stop() }()
	dev.block.Store(true)
	resized := make(chan error, 1)
	go func() { resized <- b.ResizeRDPDesktop(1600, 900) }()
	select {
	case <-dev.switching:
	case <-time.After(time.Second):
		t.Fatal("display switch did not begin")
	}
	input := make(chan error, 1)
	go func() { input <- b.KeyDown(4) }()
	select {
	case err := <-input:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("native display switch blocked the RDP input handler")
	}
	if <-keys != 4 {
		t.Fatal("input was routed to the wrong machine")
	}
	unblock()
	if err := <-resized; err != nil {
		t.Fatal(err)
	}
	dev.waitForRead(t)
	b.OnClientDisconnected()
}

func (d *desktopCapture) SetDesktopSize(size capture.DesktopSize) (capture.DesktopSize, error) {
	d.requests = append(d.requests, size)
	if d.failSet {
		return capture.DesktopSize{}, errors.New("no eligible mode")
	}
	d.changed = true
	if d.selected.Width > 0 && d.selected.Height > 0 {
		return d.selected, nil
	}
	return capture.DesktopSize{Width: 2048, Height: 1280}, nil
}
func (d *desktopCapture) RestoreDesktopSize() error {
	if d.changed {
		d.restores++
		d.changed = false
	}
	return nil
}
func (*desktopCapture) DesktopDisplayID() uint32 { return 42 }
func (d *desktopCapture) OpenWithFormat(w, h, fps int) error {
	if d.failOpen {
		return errors.New("capture unavailable")
	}
	return d.directCapture.OpenWithFormat(w, h, fps)
}

func TestGameStreamDesktopAutoAndNative(t *testing.T) {
	for _, tc := range []struct {
		name string
		auto bool
		fps  int
	}{
		{"native", false, 0}, {"auto", true, 0}, {"auto@30", true, 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev := &desktopCapture{}
			b := NewBridge([]*MachineSession{{Capture: dev, AutoResolution: tc.auto, FPS: tc.fps}}, "alt", 30, 0, 0)
			if err := b.Start(); err != nil {
				t.Fatal(err)
			}
			defer b.Stop()
			if err := b.StartGameStream(0, capture.VideoStreamOptions{Width: 1920, Height: 1080, FPS: 120, Bitrate: 1000}); err != nil {
				t.Fatal(err)
			}
			dev.waitForRead(t)
			wantFPS := 120
			if tc.fps != 0 {
				wantFPS = tc.fps
			}
			if dev.profile.FPS != wantFPS {
				t.Fatalf("capture FPS=%d want=%d", dev.profile.FPS, wantFPS)
			}
			if tc.auto {
				if len(dev.requests) != 1 || dev.requests[0] != (capture.DesktopSize{Width: 1920, Height: 1080, RefreshRate: wantFPS}) {
					t.Fatalf("desktop requests: %+v", dev.requests)
				}
			} else if len(dev.requests) != 0 {
				t.Fatal("native dimensions switched desktop mode")
			}
			if err := b.RestartGameStreamVideo(); err != nil {
				t.Fatal(err)
			}
			dev.waitForRead(t)
			if dev.restores != 0 {
				t.Fatal("keyframe restart restored desktop")
			}
			b.StopGameStream()
			if dev.changed || (tc.auto && dev.restores != 1) {
				t.Fatal("desktop not restored on disconnect")
			}
		})
	}
}

func TestRDPDesktopResizeSwitchAndDisconnect(t *testing.T) {
	first, second := &desktopCapture{}, &desktopCapture{}
	b := NewBridge([]*MachineSession{{Capture: first, AutoResolution: true}, {Capture: second, AutoResolution: true}}, "alt", 30, 0, 0)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	b.StartRDPWithSize("clearcodec", 1920, 1080)
	first.waitForRead(t)
	if first.openW != 1920 || first.openH != 1080 {
		t.Fatal("initial client output size lost")
	}
	first.selected = capture.DesktopSize{Width: 1280, Height: 800}
	if err := b.ResizeRDPDesktop(1280, 720); err != nil {
		t.Fatal(err)
	}
	first.waitForRead(t)
	if first.openW != 1280 || first.openH != 720 || first.restores != 0 {
		t.Fatal("dynamic resize did not retain the original mode")
	}
	second.selected = capture.DesktopSize{Width: 1600, Height: 900}
	b.SwitchMachine(1)
	second.waitForRead(t)
	if first.restores != 1 || second.openW != 1280 || second.openH != 720 || second.requests[0].Width != 1280 || second.requests[0].Height != 720 {
		t.Fatal("machine switch lost request or restoration")
	}
	b.OnClientDisconnected()
	if second.restores != 1 {
		t.Fatal("RDP disconnect did not restore mode")
	}
	if b.ResizeRDPDesktop(800, 600) == nil {
		t.Fatal("disconnected RDP client resized desktop")
	}
}

func TestRDPAutoScalesMatchedModeToClientSize(t *testing.T) {
	for _, tc := range []struct {
		name         string
		auto, fail   bool
		wantW, wantH int
	}{
		{"matched", true, false, 1920, 1080},
		{"mode unavailable", true, true, 1920, 1080},
		{"fixed output", false, false, 1920, 1080},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev := &desktopCapture{selected: capture.DesktopSize{Width: 1650, Height: 1050}, failSet: tc.fail}
			m := &MachineSession{Capture: dev, AutoResolution: tc.auto, CapW: 1920, CapH: 1080}
			b := NewBridge([]*MachineSession{m}, "alt", 30, 0, 0)
			if err := b.Start(); err != nil {
				t.Fatal(err)
			}
			defer b.Stop()
			b.StartRDPWithSize("progressive", 1976, 952)
			dev.waitForRead(t)
			if dev.openW != tc.wantW || dev.openH != tc.wantH {
				t.Fatalf("capture=%dx%d want=%dx%d", dev.openW, dev.openH, tc.wantW, tc.wantH)
			}
			if m.CapW != 1920 || m.CapH != 1080 {
				t.Fatal("output selection overwrote configured size")
			}
			b.OnClientDisconnected()
		})
	}
}

func TestDesktopRestoredOnCaptureFailure(t *testing.T) {
	dev := &desktopCapture{failOpen: true}
	b := NewBridge([]*MachineSession{{Capture: dev, AutoResolution: true}}, "alt", 30, 0, 0)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	b.StartRDPWithSize("clearcodec", 1920, 1080)
	if dev.changed || dev.restores != 1 {
		t.Fatal("failed capture retained changed display mode")
	}
	b.OnClientDisconnected()
}

func TestRDPDesktopDPIChangeAndProtection(t *testing.T) {
	for _, auto := range []bool{false, true} {
		dev := &desktopCapture{}
		b := NewBridge([]*MachineSession{{Capture: dev, AutoResolution: auto}}, "alt", 30, 0, 0)
		if err := b.Start(); err != nil {
			t.Fatal(err)
		}
		initial := capture.DesktopSize{Width: 1920, Height: 1080, ScalePercent: 150}
		b.StartRDPWithDesktop("clearcodec", initial)
		dev.waitForRead(t)
		if auto && (len(dev.requests) != 1 || dev.requests[0] != initial) {
			t.Fatal("initial DPI not passed to desktop")
		}
		updated := initial
		updated.ScalePercent = 200
		if err := b.ResizeRDPDesktopWithOptions(updated); err != nil {
			t.Fatal(err)
		}
		if auto {
			dev.waitForRead(t)
			if len(dev.requests) != 2 || dev.requests[1] != updated || dev.restores != 0 {
				t.Fatal("DPI-only update ignored or restored original desktop prematurely")
			}
		}
		if err := b.ResizeRDPDesktopWithOptions(updated); err != nil {
			t.Fatal(err)
		}
		if auto && len(dev.requests) != 2 {
			t.Fatal("identical desktop request reopened capture")
		}
		updated.Width, updated.Height = 1280, 720
		if err := b.ResizeRDPDesktopWithOptions(updated); err != nil {
			t.Fatal(err)
		}
		if auto {
			dev.waitForRead(t)
			if len(dev.requests) != 3 || dev.requests[2].ScalePercent != 100 {
				t.Fatal("low-resolution DPI protection not applied")
			}
		}
		updated.ScalePercent = 150
		if err := b.ResizeRDPDesktopWithOptions(updated); err != nil {
			t.Fatal(err)
		}
		if auto && len(dev.requests) != 3 {
			t.Fatal("low-resolution DPI update reopened capture despite forced 100% scaling")
		}
		if !auto && len(dev.requests) != 0 {
			t.Fatal("native desktop followed client DPI")
		}
		b.OnClientDisconnected()
		if auto && dev.restores != 1 {
			t.Fatal("DPI desktop not restored on disconnect")
		}
		b.Stop()
	}
}

func TestConfiguredVideoBitratesRemainProtocolSpecific(t *testing.T) {
	dev := &bitrateDesktopCapture{}
	b := NewBridge([]*MachineSession{{Capture: dev, CapW: 1280, CapH: 720, FPS: 60}}, "alt", 0, 0, 0)
	b.SetWebBitrate(25000)
	b.SetVNCBitrate(17000)
	b.SetRDPBitrate(19000)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	b.StartRDPWithSize("h264", 1280, 720)
	dev.waitForRead(t)
	if dev.openedBitrate != 19000 {
		t.Fatal("RDP lost its configured bitrate")
	}
	b.OnClientDisconnected()
	if err := b.StartWeb(0); err != nil {
		t.Fatal(err)
	}
	b.OnClientConnected()
	b.SetVideoWantsH264(true)
	dev.waitForRead(t)
	if dev.openedBitrate != 25000 {
		t.Fatalf("Web bitrate=%d", dev.openedBitrate)
	}
	b.OnClientDisconnected()
	b.ClaimVNC()
	b.SetVideoWantsH264(true)
	b.OnClientConnected()
	dev.waitForRead(t)
	if dev.openedBitrate != 17000 {
		t.Fatalf("VNC bitrate=%d", dev.openedBitrate)
	}
	b.OnClientDisconnected()
	profile := capture.VideoStreamOptions{Codec: "h264", Width: 640, Height: 480, FPS: 60, Bitrate: 4000}
	if err := b.StartGameStream(0, profile); err != nil {
		t.Fatal(err)
	}
	dev.waitForRead(t)
	if dev.openedBitrate != 0 || dev.profile == nil || *dev.profile != profile {
		t.Fatal("server bitrate overrode GameStream's client profile")
	}
}
