package bridge

import (
	"errors"
	"sync"
	"testing"
	"time"

	"anykvm/internal/capture"
)

type blockingCapture struct {
	mu                    sync.Mutex
	stop                  chan struct{}
	entered               chan struct{}
	opens                 int
	decode                bool
	codec                 string
	reads                 int
	openW, openH, openFPS int
}

func (d *blockingCapture) Open() error { return d.OpenWithFormat(0, 0, 0) }
func (d *blockingCapture) OpenWithFormat(w, h, fps int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stop = make(chan struct{})
	d.entered = make(chan struct{})
	d.opens++
	d.openW, d.openH, d.openFPS = w, h, fps
	d.reads = 0
	return nil
}
func (d *blockingCapture) Capture() (*capture.Frame, error) {
	d.mu.Lock()
	stop := d.stop
	d.reads++
	if d.reads == 1 {
		close(d.entered)
	}
	d.mu.Unlock()
	<-stop
	return nil, errors.New("closed")
}
func (d *blockingCapture) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stop != nil {
		select {
		case <-d.stop:
		default:
			close(d.stop)
		}
	}
	return nil
}
func (d *blockingCapture) SetDecodeImage(v bool)  { d.mu.Lock(); d.decode = v; d.mu.Unlock() }
func (*blockingCapture) SetJPEGQuality(int)       {}
func (d *blockingCapture) SetVideoCodec(v string) { d.mu.Lock(); d.codec = v; d.mu.Unlock() }
func (d *blockingCapture) waitForRead(t *testing.T) {
	t.Helper()
	d.mu.Lock()
	entered := d.entered
	d.mu.Unlock()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("capture did not start")
	}
}

func TestSwitchWaitsForOldCaptureAndAppliesCodec(t *testing.T) {
	first, second := &blockingCapture{}, &blockingCapture{}
	b := NewBridge([]*MachineSession{{Capture: first}, {Capture: second}}, "alt", 30, 0, 0)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	b.ClaimVNC()
	b.SetVideoWantsH264(true) // The source supports codec selection without chroma selection.
	b.OnClientConnected()
	first.waitForRead(t)
	b.SwitchMachine(1)
	second.waitForRead(t)
	first.mu.Lock()
	oldReads := first.reads
	first.mu.Unlock()
	second.mu.Lock()
	codec := second.codec
	second.mu.Unlock()
	if oldReads != 1 {
		t.Fatalf("old capture read count = %d", oldReads)
	}
	if codec != "h264" {
		t.Fatalf("new device codec = %q", codec)
	}
	b.OnClientDisconnected()
	second.mu.Lock()
	reads := second.reads
	second.mu.Unlock()
	if reads != 1 {
		t.Fatalf("new capture read count = %d, possible stale worker", reads)
	}
}

func TestHotkeyWithMissingHIDDoesNotPanic(t *testing.T) {
	b := NewBridge([]*MachineSession{{}, {}}, "alt", 30, 0, 0)
	b.KeyDown(hidKeyAltLeft)
	if err := b.KeyDown(0x1f); err != nil {
		t.Fatal(err)
	}
	if b.active != 1 {
		t.Fatal("machine did not switch")
	}
}

func TestFramebufferSizeDuringCaptureResize(t *testing.T) {
	m := &MachineSession{CapW: 640, CapH: 480}
	b := NewBridge([]*MachineSession{m}, "alt", 30, 0, 0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			b.mu.Lock()
			m.FallbackW, m.FallbackH = 100+i, 200+i
			b.mu.Unlock()
		}
	}()
	for i := 0; i < 1000; i++ {
		w, h := b.framebufferSize()
		if (w != 640 || h != 480) && h-w != 100 {
			t.Errorf("inconsistent size: %dx%d", w, h)
		}
	}
	<-done
}

func TestFramebufferSizeRequiresBothDimensions(t *testing.T) {
	m := &MachineSession{FallbackW: 1920, CapW: 1280, CapH: 720}
	b := NewBridge([]*MachineSession{m}, "alt", 30, 0, 0)
	if w, h := b.framebufferSize(); w != 1280 || h != 720 {
		t.Fatalf("size = %dx%d", w, h)
	}
}

// Synchronous disconnect callbacks exercise the same takeover ordering used
// by the protocol servers without touching a real capture or HID device.
type takeoverServer struct {
	close  func()
	frames []Frame
}

func (s *takeoverServer) PushFrame(f Frame)            { s.frames = append(s.frames, f) }
func (*takeoverServer) UpdateFramebufferSize(int, int) {}
func (s *takeoverServer) CloseClients() {
	if s.close != nil {
		s.close()
	}
}
func TestGameStreamTakeoverAndFallback(t *testing.T) {
	b := NewBridge([]*MachineSession{{Name: "No signal", FallbackW: 320, FallbackH: 240}}, "alt", 30, 0, 0)
	gs := &takeoverServer{close: b.StopGameStream}
	ws := &takeoverServer{close: b.OnClientDisconnected}
	b.SetGameStream(gs, nil)
	b.SetWS(ws, nil)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	b.ClaimWS()
	b.OnClientConnected()
	if err := b.StartGameStream(0, capture.VideoStreamOptions{Width: 1280, Height: 720, FPS: 30, Bitrate: 2000}); err != nil {
		t.Fatal(err)
	}
	if b.consumers != 1 || b.activeProto != "gamestream" || !b.needPixels.Load() {
		t.Fatalf("incorrect GameStream ownership: %d/%s", b.consumers, b.activeProto)
	}
	if len(gs.frames) == 0 || gs.frames[len(gs.frames)-1].Format != capture.FormatRaw {
		t.Fatal("missing raw no-signal frame")
	}
	b.ClaimWS()
	b.OnClientConnected()
	if b.consumers != 1 || b.activeProto != "ws" {
		t.Fatalf("incorrect Web takeover: %d/%s", b.consumers, b.activeProto)
	}
	b.OnClientDisconnected()
	if b.consumers != 0 || b.activeProto != "" {
		t.Fatal("session not cleaned up")
	}
}

type rdpCapture struct {
	blockingCapture
	chroma string
}

func (d *rdpCapture) SetVideoChroma(chroma string) { d.mu.Lock(); d.chroma = chroma; d.mu.Unlock() }

type nv12RDPCapture struct {
	rdpCapture
	supported bool
}

func (d *nv12RDPCapture) SupportsVideoCodec(codec string) bool { return codec != "nv12" || d.supported }

func TestRDPPlanarPrefersNativeNV12(t *testing.T) {
	first, second := &nv12RDPCapture{supported: true}, &nv12RDPCapture{}
	b := NewBridge([]*MachineSession{{Capture: first}, {Capture: second}}, "alt", 30, 0, 0)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	b.StartRDPWithCodec("planar")
	first.waitForRead(t)
	first.mu.Lock()
	codec := first.codec
	first.mu.Unlock()
	if codec != "nv12" {
		t.Fatalf("native Planar capture selected %q", codec)
	}
	b.SwitchMachine(1)
	second.waitForRead(t)
	second.mu.Lock()
	codec = second.codec
	second.mu.Unlock()
	if codec != "rawvideo" {
		t.Fatalf("BGRA fallback selected %q", codec)
	}
	b.SwitchMachine(0)
	first.mu.Lock()
	codec = first.codec
	first.mu.Unlock()
	if codec != "nv12" {
		t.Fatal("NV12 preference lost on machine switch")
	}
	b.OnClientDisconnected()
	b.StartRDPWithCodec("clearcodec")
	first.mu.Lock()
	codec = first.codec
	first.mu.Unlock()
	if codec != "rawvideo" {
		t.Fatal("Planar NV12 preference leaked into ClearCodec")
	}
	b.OnClientDisconnected()
}

func TestRDPBitmapCaptureAndMachineSwitch(t *testing.T) {
	for _, codec := range []string{"clearcodec", "planar"} {
		t.Run(codec, func(t *testing.T) {
			first, second := &rdpCapture{}, &rdpCapture{}
			b := NewBridge([]*MachineSession{{Capture: first}, {Capture: second}}, "alt", 30, 0, 0)
			if err := b.Start(); err != nil {
				t.Fatal(err)
			}
			defer b.Stop()
			b.StartRDPWithCodec(codec)
			first.waitForRead(t)
			b.SwitchMachine(1)
			second.waitForRead(t)
			for _, dev := range []*rdpCapture{first, second} {
				dev.mu.Lock()
				selected, decode := dev.codec, dev.decode
				dev.mu.Unlock()
				if selected != "rawvideo" || !decode {
					t.Fatalf("bitmap capture selected %q, decode=%t", selected, decode)
				}
			}
			b.OnClientDisconnected()
			if b.capturing || b.consumers != 0 {
				t.Fatal("bitmap capture consumer leaked")
			}
		})
	}
}

func TestRDPTakeoverMachineSwitchAndRelease(t *testing.T) {
	first, second := &rdpCapture{}, &rdpCapture{}
	b := NewBridge([]*MachineSession{{Capture: first}, {Capture: second}}, "alt", 30, 0, 0)
	b.SetWS(&takeoverServer{close: b.OnClientDisconnected}, nil)
	b.SetRDP(&takeoverServer{close: b.OnClientDisconnected})
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	b.ClaimWS()
	b.OnClientConnected()
	first.waitForRead(t)
	b.StartRDP()
	first.waitForRead(t)
	first.mu.Lock()
	codec, chroma, decode := first.codec, first.chroma, first.decode
	first.mu.Unlock()
	if b.activeProto != "rdp" || b.consumers != 1 || codec != "h264" || chroma != "420" || decode {
		t.Fatalf("RDP capture ownership: %s %d %s %s %v", b.activeProto, b.consumers, codec, chroma, decode)
	}
	b.SwitchMachine(1)
	second.waitForRead(t)
	second.mu.Lock()
	codec, chroma = second.codec, second.chroma
	second.mu.Unlock()
	if codec != "h264" || chroma != "420" {
		t.Fatal("machine switch lost AVC420 source selection")
	}
	b.ClaimWS()
	b.OnClientConnected()
	if b.activeProto != "ws" || b.consumers != 1 {
		t.Fatal("RDP did not yield to Web")
	}
	b.OnClientDisconnected()
	if b.capturing || b.consumers != 0 {
		t.Fatal("RDP consumer leaked")
	}
}
func TestGameStreamMachineSelectionAndCaptureShutdown(t *testing.T) {
	first, second := &blockingCapture{}, &blockingCapture{}
	b := NewBridge([]*MachineSession{{Name: "A", Capture: first}, {Name: "B", Capture: second}}, "alt", 30, 0, 0)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	if err := b.StartGameStream(1, capture.VideoStreamOptions{Width: 1280, Height: 720, FPS: 30, Bitrate: 2000}); err != nil {
		t.Fatal(err)
	}
	second.waitForRead(t)
	if b.active != 1 || !second.decode || first.opens != 0 {
		t.Fatal("wrong capture source or decode mode")
	}
	b.StopGameStream()
	if b.capturing || b.consumers != 0 {
		t.Fatal("capture remained active")
	}
	if err := b.StartGameStream(0, capture.VideoStreamOptions{Width: 1280, Height: 720, FPS: 30, Bitrate: 2000}); err != nil {
		t.Fatal(err)
	}
	first.waitForRead(t)
	b.StopGameStream()
}

type directCapture struct {
	blockingCapture
	profile *capture.VideoStreamOptions
}

type scopedCapture struct {
	blockingCapture
	gameStream bool
	bitrate    int
}

func (d *scopedCapture) SetGameStreamCaptureOptions(profile *capture.VideoStreamOptions) {
	d.gameStream = profile != nil
	d.bitrate = 0
	if profile != nil {
		d.bitrate = profile.Bitrate
	}
}

func TestGameStreamCaptureSettingsRestoreOnTakeover(t *testing.T) {
	dev := &scopedCapture{}
	b := NewBridge([]*MachineSession{{Capture: dev}}, "alt", 30, 0, 0)
	b.SetGameStream(&takeoverServer{close: b.StopGameStream}, nil)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	profile := capture.VideoStreamOptions{Codec: "h264", Width: 1280, Height: 720, FPS: 30, Bitrate: 1600}
	if err := b.StartGameStream(0, profile); err != nil {
		t.Fatal(err)
	}
	dev.waitForRead(t)
	if !dev.gameStream || dev.bitrate != profile.Bitrate || dev.codec != "rawvideo" {
		t.Fatal("capture scope missing or wrongly treated as a direct encoder")
	}
	if err := b.RestartGameStreamVideo(); err != nil {
		t.Fatal(err)
	}
	dev.waitForRead(t)
	if !dev.gameStream || dev.bitrate != profile.Bitrate {
		t.Fatal("capture settings lost on keyframe restart")
	}
	b.ClaimWS()
	b.OnClientConnected()
	dev.waitForRead(t)
	if dev.gameStream || dev.bitrate != 0 {
		t.Fatal("GameStream capture settings leaked into Web capture")
	}
	b.OnClientDisconnected()
	b.ClaimVNC()
	b.OnClientConnected()
	dev.waitForRead(t)
	if dev.gameStream {
		t.Fatal("GameStream capture settings leaked into VNC capture")
	}
	b.OnClientDisconnected()
}

func (d *directCapture) SetVideoStreamOptions(o *capture.VideoStreamOptions) { d.profile = o }

func (d *directCapture) SupportsGameStreamHEVC() bool { return true }
func (d *directCapture) SupportsGameStreamAV1() bool  { return true }

type hevcOnlyCapture struct{ directCapture }

func (*hevcOnlyCapture) SupportsVideoCodec(codec string) bool {
	return codec == "hevc" || codec == "rawvideo" || codec == ""
}

func TestGameStreamRejectsUnsupportedNativeCodec(t *testing.T) {
	for _, codec := range []string{"", "h264", "av1"} {
		t.Run(codec, func(t *testing.T) {
			dev := &hevcOnlyCapture{}
			b := NewBridge([]*MachineSession{{Capture: dev}}, "alt", 30, 0, 0)
			evicted := false
			b.SetGameStream(&takeoverServer{close: func() { evicted = true }}, nil)
			if err := b.StartGameStream(0, capture.VideoStreamOptions{Codec: codec}); err == nil {
				t.Fatal("unsupported native codec accepted")
			}
			if evicted || b.consumers != 0 || b.gsVideo != nil || dev.opens != 0 {
				t.Fatal("rejected codec changed session ownership or opened capture")
			}
		})
	}
}

func TestGameStreamCannotSwitchToUnsupportedNativeCodec(t *testing.T) {
	dev := &hevcOnlyCapture{}
	b := NewBridge([]*MachineSession{{Capture: &directCapture{}}, {Capture: dev}}, "alt", 30, 0, 0)
	for _, codec := range []string{"", "h264"} {
		b.gsVideo = &capture.VideoStreamOptions{Codec: codec}
		b.SwitchMachine(1)
		if b.active != 0 || dev.opens != 0 {
			t.Fatal("switched a GameStream session to an unsupported native codec")
		}
	}
}

func TestGameStreamDirectCaptureRestartAndRestore(t *testing.T) {
	for _, codec := range []string{"h264", "hevc", "av1"} {
		t.Run(codec, func(t *testing.T) { testGameStreamDirectCaptureRestartAndRestore(t, codec) })
	}
}
func testGameStreamDirectCaptureRestartAndRestore(t *testing.T, codec string) {
	dev := &directCapture{}
	b := NewBridge([]*MachineSession{{Capture: dev}}, "alt", 30, 0, 0)
	b.SetGameStream(&takeoverServer{close: b.StopGameStream}, nil)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	profile := capture.VideoStreamOptions{Codec: codec, Width: 1280, Height: 720, FPS: 30, Bitrate: 1600}
	if err := b.StartGameStream(0, profile); err != nil {
		t.Fatal(err)
	}
	dev.waitForRead(t)
	if dev.profile == nil || *dev.profile != profile || dev.codec != codec || dev.decode {
		t.Fatalf("direct profile not applied: %+v codec=%s decode=%v", dev.profile, dev.codec, dev.decode)
	}
	if b.GameStreamCanEncodePlaceholder() {
		t.Fatal("native passthrough was mistaken for a local output encoder")
	}
	if err := b.RestartGameStreamVideo(); err != nil {
		t.Fatal(err)
	}
	dev.waitForRead(t)
	if dev.opens != 2 || b.consumers != 1 {
		t.Fatal("restart changed ownership or did not reopen")
	}
	b.ClaimWS()
	b.OnClientConnected()
	dev.waitForRead(t)
	if dev.profile != nil || dev.codec != "" {
		t.Fatal("GameStream settings leaked into Web capture")
	}
	b.OnClientDisconnected()
}

type nativeKeyframeCapture struct {
	directCapture
	requests int
}

func (d *nativeKeyframeCapture) RequestKeyframe() error { d.requests++; return nil }

func TestWebNativeKeyframeDoesNotReopen(t *testing.T) {
	dev := &nativeKeyframeCapture{}
	b := NewBridge([]*MachineSession{{Capture: dev}}, "alt", 30, 0, 0)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	b.ClaimWS()
	b.OnClientConnected()
	dev.waitForRead(t)
	b.RequestVideoKeyframe()
	if dev.requests != 1 || dev.opens != 1 || b.consumers != 1 {
		t.Fatalf("Web keyframe reopened capture: requests=%d opens=%d consumers=%d", dev.requests, dev.opens, b.consumers)
	}
	b.OnClientDisconnected()
}

func TestGameStreamNativeKeyframeDoesNotReopen(t *testing.T) {
	dev := &nativeKeyframeCapture{}
	b := NewBridge([]*MachineSession{{Capture: dev}}, "alt", 30, 0, 0)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	if err := b.StartGameStream(0, capture.VideoStreamOptions{Width: 320, Height: 240, FPS: 30, Bitrate: 1000}); err != nil {
		t.Fatal(err)
	}
	dev.waitForRead(t)
	handled, err := b.RequestGameStreamKeyframe()
	if err != nil || !handled || dev.requests != 1 || dev.opens != 1 || b.consumers != 1 {
		t.Fatalf("native refresh reopened capture: handled=%v err=%v requests=%d opens=%d", handled, err, dev.requests, dev.opens)
	}
}

type stoppedCapture struct{ blockingCapture }

type passiveKeyframeCapture struct{ directCapture }

func (*passiveKeyframeCapture) CanRestartForKeyframe() bool { return false }

func TestGameStreamKeyframeRestartPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		device capture.Device
		want   bool
	}{
		{"legacy capture", &directCapture{}, true},
		{"sender-driven capture", &passiveKeyframeCapture{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := NewBridge([]*MachineSession{{Capture: tc.device}}, "alt", 60, 0, 0)
			if got := b.GameStreamCanRestartForKeyframe(); got != tc.want {
				t.Fatalf("restart allowed=%v, want %v", got, tc.want)
			}
		})
	}
}

func (d *stoppedCapture) Capture() (*capture.Frame, error) {
	d.mu.Lock()
	d.reads++
	d.mu.Unlock()
	return nil, capture.ErrStreamStopped
}
func TestStoppedCaptureClosesWithoutRetryingDeadSession(t *testing.T) {
	d := &stoppedCapture{}
	b := NewBridge([]*MachineSession{{Capture: d}}, "alt", 30, 0, 0)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	b.ClaimVNC()
	b.OnClientConnected()
	deadline := time.Now().Add(time.Second)
	for {
		b.mu.Lock()
		stopped := !b.capturing && b.run == nil
		b.mu.Unlock()
		if stopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stopped capture did not close")
		}
		time.Sleep(time.Millisecond)
	}
	// Wait for cleanup to complete, not merely for the run pointer to clear.
	b.captureMu.Lock()
	b.captureMu.Unlock()
	d.mu.Lock()
	reads, stop := d.reads, d.stop
	d.mu.Unlock()
	if reads != 1 {
		t.Fatalf("read failed session %d times", reads)
	}
	select {
	case <-stop:
	default:
		t.Fatal("native capture not closed")
	}
	b.OnClientDisconnected()
}
func TestOldCaptureFailureCannotCloseNewRun(t *testing.T) {
	b := NewBridge(nil, "alt", 30, 0, 0)
	old, current := &captureRun{}, &captureRun{}
	b.run = current
	b.capturing = true
	b.closeFailedCapture(old)
	if b.run != current || !b.capturing {
		t.Fatal("old failure closed a new capture")
	}
}

func TestGameStreamRejectsUnsupportedHEVCSource(t *testing.T) {
	b := NewBridge([]*MachineSession{{Capture: &blockingCapture{}}}, "alt", 30, 0, 0)
	if err := b.StartGameStream(0, capture.VideoStreamOptions{Codec: "hevc", Width: 1280, Height: 720, FPS: 30, Bitrate: 1600}); err == nil {
		t.Fatal("accepted unsupported HEVC")
	}
	if b.consumers != 0 || b.activeProto != "" {
		t.Fatal("failed request changed ownership")
	}
}

// Model a fixed-mode capture card whose encoded output differs from both the
// configured input size and the client's requested output size.
type fixedModeCapture struct{ directCapture }

func (d *fixedModeCapture) Capture() (*capture.Frame, error) {
	d.mu.Lock()
	if d.reads == 0 {
		d.reads++
		close(d.entered)
		d.mu.Unlock()
		return &capture.Frame{Format: capture.FormatH264, Width: 1920, Height: 1080, Data: []byte{0, 0, 0, 1, 0x65}}, nil
	}
	d.mu.Unlock()
	return d.blockingCapture.Capture()
}
func TestGameStreamPreservesCaptureModeAcrossRestart(t *testing.T) {
	dev := &fixedModeCapture{}
	machine := &MachineSession{Capture: dev, CapW: 2560, CapH: 1440, FPS: 60}
	b := NewBridge([]*MachineSession{machine}, "alt", 60, 0, 0)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	profile := capture.VideoStreamOptions{Width: 1280, Height: 720, FPS: 30, Bitrate: 1000}
	if err := b.StartGameStream(0, profile); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		dev.waitForRead(t)
		deadline := time.Now().Add(time.Second)
		for {
			b.mu.Lock()
			ready := machine.FallbackW == 1920
			b.mu.Unlock()
			if ready {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("no captured frame")
			}
			time.Sleep(time.Millisecond)
		}
		if dev.openW != 2560 || dev.openH != 1440 || dev.openFPS != 60 {
			t.Fatalf("client settings replaced capture mode: %dx%d@%d", dev.openW, dev.openH, dev.openFPS)
		}
		if dev.profile == nil || *dev.profile != profile {
			t.Fatal("output profile lost")
		}
		if attempt == 0 {
			if err := b.RestartGameStreamVideo(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestGameStreamRejectsUnsupportedAV1Source(t *testing.T) {
	b := NewBridge([]*MachineSession{{Capture: &blockingCapture{}}}, "alt", 30, 0, 0)
	if b.StartGameStream(0, capture.VideoStreamOptions{Codec: "av1", Width: 1280, Height: 720, FPS: 30, Bitrate: 4000}) == nil {
		t.Fatal("unsupported AV1 accepted")
	}
}
func TestBrowserAV1Fallback(t *testing.T) {
	for _, hardware := range []bool{false, true} {
		var dev capture.Device = &blockingCapture{}
		if hardware {
			dev = &directCapture{}
		}
		b := NewBridge([]*MachineSession{{Capture: dev}}, "alt", 30, 0, 0)
		b.SetVideoCodecs(true, false, true)
		if got := b.videoCodec; (got == "av1") != hardware || (!hardware && got != "h264") {
			t.Fatalf("hardware=%v codec=%s", hardware, got)
		}
	}
}

type hevcProbeCapture struct {
	blockingCapture
	supported bool
	probes    int
}

func (d *hevcProbeCapture) SupportsVideoCodec(codec string) bool {
	if codec == "hevc" {
		d.probes++
		return d.supported
	}
	return true
}

func TestBrowserHEVCProbeAndFallback(t *testing.T) {
	for _, hardware := range []bool{false, true} {
		dev := &hevcProbeCapture{supported: hardware}
		b := NewBridge([]*MachineSession{{Capture: dev}}, "alt", 30, 0, 0)
		b.SetVideoCodecs(true, false, false)
		if dev.probes != 0 || b.videoCodec != "h264" {
			t.Fatal("disabled HEVC probed hardware or changed H.264")
		}
		b.SetVideoCodecs(true, true, false)
		if dev.probes != 1 || (b.videoCodec == "hevc") != hardware || (!hardware && b.videoCodec != "h264") {
			t.Fatalf("hardware=%v probes=%d codec=%s", hardware, dev.probes, b.videoCodec)
		}
	}
}

type optionalCodecProbeCapture struct {
	blockingCapture
	available bool
	probes    map[string]int
}

func (d *optionalCodecProbeCapture) SupportsVideoCodec(codec string) bool {
	if codec == "hevc" || codec == "av1" {
		d.probes[codec]++
		return d.available
	}
	return true
}

func (d *optionalCodecProbeCapture) SupportsGameStreamAV1() bool {
	d.probes["av1"]++
	return d.available
}

func TestBrowserOptionalCodecProbesFollowWants(t *testing.T) {
	for _, available := range []bool{false, true} {
		for _, hevc := range []bool{false, true} {
			for _, av1 := range []bool{false, true} {
				dev := &optionalCodecProbeCapture{available: available, probes: make(map[string]int)}
				b := NewBridge([]*MachineSession{{Capture: dev}}, "alt", 30, 0, 0)
				if len(dev.probes) != 0 {
					t.Fatal("bridge startup probed optional codecs")
				}
				b.SetVideoCodecs(true, hevc, av1)
				if !hevc && dev.probes["hevc"] != 0 || !av1 && dev.probes["av1"] != 0 {
					t.Fatalf("disabled codec probed: hevc=%v av1=%v probes=%v", hevc, av1, dev.probes)
				}
				want := "h264"
				if available && av1 {
					want = "av1"
				} else if available && hevc {
					want = "hevc"
				}
				if b.videoCodec != want {
					t.Fatalf("available=%v hevc=%v av1=%v codec=%s want=%s", available, hevc, av1, b.videoCodec, want)
				}
				clear(dev.probes)
				b.SetVideoCodecs(true, false, false)
				if len(dev.probes) != 0 || b.videoCodec != "h264" {
					t.Fatal("cleared optional codec wants still probed hardware")
				}
			}
		}
	}
}
