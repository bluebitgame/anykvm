package bridge

import (
	"anykvm/internal/framebuf"
	"anykvm/internal/framerate"
	"anykvm/internal/gpuconv"
	"anykvm/internal/hid"
	"anykvm/internal/jpegcodec"
	"anykvm/internal/videotiming"
	"errors"
	"fmt"
	"image"
	"log"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"anykvm/internal/capture"
	"anykvm/internal/vnc"
)

// HIDSender is the common keyboard/mouse interface shared across VNC, RDP, and
// WebSocket servers. Both *hid.Device and *scrcpy.Session satisfy it.
type HIDSender interface {
	KeyDown(keycode uint8) error
	KeyUp() error
	KeyRelease(keycode uint8) error
	MouseMove(dx, dy int) error
	MouseMoveRel(dx, dy int) error
	MouseButton(button int, down bool) error
	MouseWheel(delta int) error
}

// SlotSelecter is implemented by BLE HID backends that support per-slot
// profile management (nRF52840).  SwitchMachine calls SelectSlot when
// the active target changes so the dongle switches its active BLE profile.
type SlotSelecter interface {
	SelectSlot(slot int) error
}

// MachineSession is the runtime state for one controlled target.
type MachineSession struct {
	Name            string
	Capture         capture.Device
	AudioDev        capture.AudioDevice
	HID             HIDSender
	FPS             int
	CapW            int // configured input size; never overwritten by encoded output
	CapH            int
	AutoResolution  bool   // video.mode: auto; omitted dimensions retain the device mode
	MouseMode       string // "auto" | "absolute" | "relative"
	WheelScale      float64
	wheelMu         sync.Mutex
	wheelRemainder  float64
	CursorRendering string // client (default) or remote

	// Fallback dimensions for when capture fails.
	FallbackW int
	FallbackH int
}

type Frame struct {
	Surface *gpuconv.Surface // Immutable GPU snapshot, owned by Buffer; no CPU Data.
	Timing  videotiming.Delivery
	// Buffer owns immutable Data when non-nil. Frame copies borrow this reference.
	Buffer                    *framebuf.Buffer
	PreserveSource            bool
	BufferH264Presentation    bool // explicit source opt-in, only for native H.264
	CaptureScalesOutput       bool // scalable backend follows output requests (not fixed-source AUs)
	OutputMaxW, OutputMaxH    int  // configured Target ceiling; zero means source size
	CaptureFPS                int
	CaptureRate               framerate.Rate
	CapturedAt                time.Time
	Format                    capture.VideoFormat
	PixFmt                    string
	Data                      []byte
	Width                     int
	Height                    int
	Rotation                  int // clockwise display rotation, independent of encoded dimensions
	SourceWidth, SourceHeight int
	ContentRect               image.Rectangle // desktop content in frame pixels; empty when unspecified
}

// AudioSink receives PCM audio, or compressed audio if it also implements
// EncodedAudioSink and accepts the source format.
type AudioSink interface {
	PushAudio(capture.AudioFrame)
}

type FrameServer interface {
	PushFrame(Frame)
	UpdateFramebufferSize(width, height int)
	CloseClients()
}

type Bridge struct {
	vncSrv          *vnc.Server
	wsSrv           FrameServer
	gsSrv           FrameServer
	rdpSrv          FrameServer
	viewerSrv       FrameServer
	viewerAudioSink AudioSink
	viewerMu        sync.RWMutex

	machines []*MachineSession
	active   int // index into machines
	fps      int // configured FPS (from first target initially)

	hotkey *HotkeyMatcher

	activeAudioSink AudioSink
	vncAudioSink    AudioSink
	wsAudioSink     AudioSink
	gsAudioSink     AudioSink
	rdpAudioSink    AudioSink

	gsVideo                *capture.VideoStreamOptions // protected by captureMu
	rdpEncoderBufferFrames int                         // native RDP encoder in-flight limit, protected by captureMu
	vncBitrate             int                         // encoded VNC video bitrate, protected by captureMu
	webBitrate             int                         // encoded Web video bitrate, protected by captureMu
	rdpBitrate             int                         // native RDP H.264 bitrate in kbps, protected by captureMu
	desktopSize            *capture.DesktopSize        // controlling client's request, protected by captureMu
	captureMu              sync.Mutex                  // serializes open, close, restart, and target switching
	mu                     sync.Mutex
	running                bool
	capturing              bool
	audioOn                bool
	consumers              int
	clientActivity         func(bool) // nonblocking observer, called with mu held
	run                    *captureRun

	// Global single-client arbitration + lazy-decode state.
	sessionMu   sync.Mutex
	activeProto string
	needPixels  atomic.Bool

	// Capture output codec selection (follows the single active client).
	codecMu        sync.Mutex
	wantH264       bool
	wantAV1        bool
	wantHEVC       bool
	wantNV12       bool // Planar prefers native subsampled pixels when available.
	wantSurface    bool // Progressive compute can consume immutable GPU snapshots.
	videoCodec     string
	webJPEGQuality int // protected by captureMu; applied on future capture opens
}

type captureRun struct {
	stop chan struct{}
	wg   sync.WaitGroup
	// Serializes recovery opens with the final close during teardown. Workers
	// must never take captureMu, since teardown holds it while waiting for them.
	reopenMu           sync.Mutex
	width, height, fps int
}

func NewBridge(machines []*MachineSession, hotkeyMod string, fps, capW, capH int) *Bridge {
	b := &Bridge{
		machines: machines,
		active:   0,
		fps:      fps,
	}
	b.hotkey = NewHotkeyMatcher(hotkeyMod, len(machines))
	b.needPixels.Store(false)
	return b
}

// SetViewer attaches the local viewer before it starts consuming frames.
func (b *Bridge) SetViewer(s FrameServer, audio AudioSink) {
	b.viewerMu.Lock()
	defer b.viewerMu.Unlock()
	b.viewerSrv, b.viewerAudioSink = s, audio
}

func (b *Bridge) StartViewer() {
	b.claimSession("viewer")
	b.SetVideoCodecs(false, false, false)
	b.SetPixelNeed(true)
	b.OnClientConnected()
}

func (b *Bridge) SetVNC(s *vnc.Server) {
	b.vncSrv = s
	b.vncAudioSink = s
}

func (b *Bridge) SetWS(s FrameServer, audio AudioSink) {
	b.wsSrv = s
	b.wsAudioSink = audio
}

func (b *Bridge) SetGameStream(s FrameServer, audio AudioSink) {
	b.gsSrv, b.gsAudioSink = s, audio
}

func (b *Bridge) SetRDP(s FrameServer) {
	b.rdpSrv = s
	b.rdpAudioSink, _ = s.(AudioSink)
}

// SetClientActivityHandler observes transitions between zero and nonzero
// streaming consumers across all protocols. Set it before starting listeners.
// The handler must not block or call back into Bridge.
func (b *Bridge) SetClientActivityHandler(handler func(bool)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clientActivity = handler
}

// RemoteControlActive excludes the local viewer and passive source listeners.
func (b *Bridge) RemoteControlActive() bool {
	b.mu.Lock()
	active := b.consumers > 0
	b.mu.Unlock()
	b.sessionMu.Lock()
	proto := b.activeProto
	b.sessionMu.Unlock()
	return active && proto != "" && proto != "viewer"
}

// setConsumersLocked keeps notifications ordered with session reservations.
func (b *Bridge) setConsumersLocked(n int) {
	if n < 0 {
		n = 0
	}
	wasActive := b.consumers > 0
	b.consumers = n
	if active := n > 0; active != wasActive && b.clientActivity != nil {
		b.clientActivity(active)
	}
}

func (b *Bridge) SetVNCBitrate(kbps int) {
	b.captureMu.Lock()
	defer b.captureMu.Unlock()
	b.vncBitrate = kbps
}

// SetWebBitrate sets the Web video target for future capture opens. Zero uses
// the encoder default; it does not override another protocol's video settings.
func (b *Bridge) SetWebBitrate(kbps int) {
	b.captureMu.Lock()
	defer b.captureMu.Unlock()
	b.webBitrate = kbps
}

// SetWebJPEGQuality sets JPEG quality for future Web capture opens.
func (b *Bridge) SetWebJPEGQuality(quality int) {
	b.captureMu.Lock()
	defer b.captureMu.Unlock()
	b.webJPEGQuality = quality
}

// SetRDPBitrate supplies the configured native encoder bitrate for future opens.
func (b *Bridge) SetRDPBitrate(kbps int) {
	b.captureMu.Lock()
	defer b.captureMu.Unlock()
	b.rdpBitrate = kbps
}

// SetRDPEncoderBufferFrames configures future native RDP capture opens.
func (b *Bridge) SetRDPEncoderBufferFrames(frames int) {
	b.captureMu.Lock()
	defer b.captureMu.Unlock()
	b.rdpEncoderBufferFrames = frames
}

// StartRDP reserves capture ownership before closing another protocol's client.
func (b *Bridge) StartRDP() {
	b.StartRDPWithCodec("h264")
}

// StartRDPWithCodec requests pixels for bitmap codecs and 4:2:0 for AVC420.
func (b *Bridge) StartRDPWithCodec(codec string) {
	b.StartRDPWithSize(codec, 0, 0)
}

// StartRDPWithSize applies the authenticated client's initial desktop request.
func (b *Bridge) StartRDPWithSize(codec string, width, height int) {
	b.StartRDPWithDesktop(codec, capture.DesktopSize{Width: width, Height: height})
}

// StartRDPWithDesktop also carries the client's desktop DPI percentage.
func (b *Bridge) StartRDPWithDesktop(codec string, size capture.DesktopSize) {
	started := time.Now()
	size = size.WithScaleProtection()
	needPixels := codec != "h264"
	b.mu.Lock()
	b.setConsumersLocked(b.consumers + 1)
	b.mu.Unlock()
	b.claimSession("rdp")
	b.captureMu.Lock()
	b.stopCaptureLocked()
	b.gsVideo = nil
	b.desktopSize = nil
	if size.Width >= 2 && size.Height >= 2 && size.Width <= 8192 && size.Height <= 8192 {
		b.desktopSize = &size
	} else {
		b.restoreDesktopsLocked()
	}
	b.needPixels.Store(needPixels)
	b.devSetDecodeImageLocked(needPixels)
	if dev := b.activeDevice(); dev != nil {
		if cs, ok := dev.(chromaSwitcher); ok {
			cs.SetVideoChroma("420")
		}
	}
	b.codecMu.Lock()
	b.wantH264, b.wantHEVC, b.wantAV1 = !needPixels, false, false
	b.wantNV12 = codec == "planar"
	mode := os.Getenv("ANYKVM_RDP_PROGRESSIVE_BACKEND")
	b.wantSurface = codec == "progressive" && (mode == "gpu" || mode == "vulkan" || mode == "d3d11")
	b.codecMu.Unlock()
	b.mu.Lock()
	b.videoCodec = ""
	b.mu.Unlock()
	b.updateCodecLocked()
	b.startCaptureLocked()
	log.Printf("bridge: RDP capture startup=%.1fms", float64(time.Since(started))/float64(time.Millisecond))
	b.captureMu.Unlock()
}

func (b *Bridge) RequestRDPKeyframe() {
	b.captureMu.Lock()
	defer b.captureMu.Unlock()
	b.sessionMu.Lock()
	active := b.activeProto == "rdp"
	b.sessionMu.Unlock()
	if !active {
		return
	}
	if dev := b.activeDevice(); dev != nil {
		if encoder, ok := dev.(interface{ RequestKeyframe() error }); ok {
			if err := encoder.RequestKeyframe(); err == nil {
				return
			}
		}
	}
	b.stopCaptureLocked()
	b.startCaptureLocked()
}

// StartGameStream reserves its consumer before evicting old clients, so their
// disconnect callbacks cannot reset the new client's codec and session state.
// FFmpeg capture sources encode directly using the negotiated stream profile.
func (b *Bridge) StartGameStream(index int, video capture.VideoStreamOptions) error {
	if index < 0 || index >= len(b.machines) {
		return fmt.Errorf("invalid target index %d", index)
	}
	if !supportsGameStreamCodec(b.machines[index].Capture, video.Codec) {
		return fmt.Errorf("target does not support GameStream codec %q", video.Codec)
	}
	if video.Codec == "av1" && !b.SupportsGameStreamAV1(index) {
		return fmt.Errorf("target does not support hardware AV1 streaming")
	}
	if video.Codec == "hevc" && !b.SupportsGameStreamHEVC(index) {
		return fmt.Errorf("target does not support hardware HEVC streaming")
	}
	b.mu.Lock()
	b.setConsumersLocked(b.consumers + 1)
	b.mu.Unlock()
	b.claimSession("gamestream")
	b.captureMu.Lock()
	b.stopCaptureLocked()
	b.gsVideo = &video
	size := (capture.DesktopSize{Width: video.Width, Height: video.Height, RefreshRate: video.FPS}).WithScaleProtection()
	b.desktopSize = &size
	b.captureMu.Unlock()
	b.codecMu.Lock()
	b.wantH264, b.wantHEVC, b.wantAV1 = false, false, false
	b.codecMu.Unlock()
	b.needPixels.Store(true)
	b.devSetDecodeImage(true)
	b.SwitchMachine(index)
	b.updateCodec()
	b.captureMu.Lock()
	b.startCaptureLocked()
	b.captureMu.Unlock()
	return nil
}

// RequestGameStreamKeyframe uses the active encoder without reopening capture.
// False means no native request was sent; check the restart policy separately.
func (b *Bridge) RequestGameStreamKeyframe() (bool, error) {
	b.captureMu.Lock()
	defer b.captureMu.Unlock()
	if b.gsVideo == nil {
		return false, fmt.Errorf("GameStream capture is not active")
	}
	if dev := b.activeDevice(); dev != nil {
		if encoder, ok := dev.(interface{ RequestKeyframe() error }); ok {
			return true, encoder.RequestKeyframe()
		}
	}
	return false, nil
}

// GameStreamCanRestartForKeyframe reports whether capture can restart for an IDR.
// Sender-driven sources must wait for a keyframe from the sender.
func (b *Bridge) GameStreamCanRestartForKeyframe() bool {
	b.captureMu.Lock()
	defer b.captureMu.Unlock()
	if policy, ok := b.activeDevice().(capture.KeyframeRestartPolicy); ok {
		return policy.CanRestartForKeyframe()
	}
	return true
}

// GameStreamCanEncodePlaceholder reports whether GameStream owns the encoder
// and can generate waiting video without restarting the source.
func (b *Bridge) GameStreamCanEncodePlaceholder() bool {
	b.captureMu.Lock()
	defer b.captureMu.Unlock()
	if b.gsVideo == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.videoCodec != "h264" && b.videoCodec != "hevc" && b.videoCodec != "av1"
}

// RestartGameStreamVideo forces a fresh IDR without changing session ownership.
func (b *Bridge) RestartGameStreamVideo() error {
	b.captureMu.Lock()
	defer b.captureMu.Unlock()
	if b.gsVideo == nil {
		return fmt.Errorf("GameStream capture is not active")
	}
	b.stopCaptureLocked()
	b.startCaptureLocked()
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.capturing {
		return fmt.Errorf("could not restart GameStream capture")
	}
	return nil
}

func (b *Bridge) StopGameStream() {
	b.captureMu.Lock()
	defer b.captureMu.Unlock()
	stopTargetHID(b.activeMachine())
	b.stopCaptureLocked()
	b.gsVideo = nil
	b.desktopSize = nil
	b.restoreDesktopsLocked()
	b.mu.Lock()
	b.setConsumersLocked(b.consumers - 1)
	b.mu.Unlock()
	b.sessionMu.Lock()
	if b.activeProto == "gamestream" {
		b.activeProto = ""
	}
	b.sessionMu.Unlock()
}

func (b *Bridge) SetAudio(dev capture.AudioDevice) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if m := b.activeMachineLocked(); m != nil {
		m.AudioDev = dev
	}
}

func (b *Bridge) activeMachine() *MachineSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.activeMachineLocked()
}

func (b *Bridge) activeMachineLocked() *MachineSession {
	if b.active >= 0 && b.active < len(b.machines) {
		return b.machines[b.active]
	}
	return nil
}

func (b *Bridge) activeDevice() capture.Device {
	if m := b.activeMachine(); m != nil {
		return m.Capture
	}
	return nil
}

// MouseMode reports the active target's mode for the local and Web viewers.
func (b *Bridge) MouseMode() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if m := b.activeMachineLocked(); m != nil && m.MouseMode == "relative" {
		return "relative"
	}
	return "absolute"
}

func (b *Bridge) MachineCount() int { return len(b.machines) }

// ReplaceMachines disconnects current clients, stops capture, and atomically
// replaces the target list. It is intended for configuration apply: target
// devices and HID backends are long-lived resources, so keeping a controller
// connected across replacement would let it drive a stale target.
func (b *Bridge) ReplaceMachines(machines []*MachineSession) error {
	if len(machines) == 0 {
		return errors.New("at least one target is required")
	}
	if b.vncSrv != nil {
		b.vncSrv.CloseClients()
	}
	if b.wsSrv != nil {
		b.wsSrv.CloseClients()
	}
	if b.gsSrv != nil {
		b.gsSrv.CloseClients()
	}
	if b.rdpSrv != nil {
		b.rdpSrv.CloseClients()
	}

	b.captureMu.Lock()
	defer b.captureMu.Unlock()
	b.stopCaptureLocked()
	b.restoreDesktopsLocked()
	b.mu.Lock()
	old := b.machines
	b.machines, b.active, b.fps, b.videoCodec = machines, 0, 0, ""
	b.gsVideo, b.desktopSize = nil, nil
	b.setConsumersLocked(0)
	b.mu.Unlock()
	b.sessionMu.Lock()
	b.activeProto = ""
	b.sessionMu.Unlock()
	b.needPixels.Store(false)
	b.devSetDecodeImageLocked(false)
	b.codecMu.Lock()
	b.wantH264, b.wantHEVC, b.wantAV1, b.wantNV12 = false, false, false, false
	b.codecMu.Unlock()

	for _, m := range old {
		if m.Capture != nil {
			_ = m.Capture.Close()
		}
		if closer, ok := m.HID.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}

	w, h := b.framebufferSize()
	if b.vncSrv != nil {
		b.vncSrv.UpdateFramebufferSize(w, h)
	}
	if b.wsSrv != nil {
		b.wsSrv.UpdateFramebufferSize(w, h)
	}
	if b.rdpSrv != nil {
		b.rdpSrv.UpdateFramebufferSize(w, h)
	}
	return nil
}

// SwitchMachine switches to the target at the given index. If the index is
// out of bounds or is the current target, this is a no-op.
func (b *Bridge) SwitchMachine(index int) {
	b.captureMu.Lock()
	defer b.captureMu.Unlock()
	b.mu.Lock()
	unchanged := index == b.active
	b.mu.Unlock()
	if index < 0 || index >= len(b.machines) || unchanged {
		return
	}

	if b.gsVideo != nil && !supportsGameStreamCodec(b.machines[index].Capture, b.gsVideo.Codec) {
		log.Printf("bridge: cannot switch %s session to unsupported target %d", b.gsVideo.Codec, index)
		return
	}
	if b.gsVideo != nil && b.gsVideo.Codec == "av1" && !b.SupportsGameStreamAV1(index) {
		log.Printf("bridge: cannot switch AV1 session to unsupported target %d", index)
		return
	}
	if b.gsVideo != nil && b.gsVideo.Codec == "hevc" && !b.SupportsGameStreamHEVC(index) {
		log.Printf("bridge: cannot switch HEVC session to unsupported target %d", index)
		return
	}
	b.stopCaptureLocked()
	if m := b.activeMachine(); m != nil {
		stopTargetHID(m)
		b.restoreDesktopLocked(m)
	}

	m := b.machines[index]
	log.Printf("bridge: switching to target %q (%d)", m.Name, index)

	b.mu.Lock()
	b.active = index
	b.fps = 0              // force recalculation from target config or capture device
	b.videoCodec = "unset" // configure the newly selected device even for MJPEG
	b.mu.Unlock()

	if ss, ok := m.HID.(SlotSelecter); ok {
		slot := 0
		if sh, ok := m.HID.(interface{ Slot() int }); ok {
			slot = sh.Slot()
		}
		if err := ss.SelectSlot(slot); err != nil {
			log.Printf("bridge: slot select %d for %q: %v", slot, m.Name, err)
		}
	}

	b.sessionMu.Lock()
	aps := b.activeProto
	b.sessionMu.Unlock()

	b.updateCodecLocked()
	if aps != "" {
		b.startCaptureLocked()
	}

	// Notify all servers about the size change.
	srvW, srvH := b.framebufferSize()
	if b.vncSrv != nil {
		b.vncSrv.UpdateFramebufferSize(srvW, srvH)
	}
	if b.wsSrv != nil {
		b.wsSrv.UpdateFramebufferSize(srvW, srvH)
	}
	if b.rdpSrv != nil {
		b.rdpSrv.UpdateFramebufferSize(srvW, srvH)
	}
}

func (b *Bridge) framebufferSize() (int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	m := b.activeMachineLocked()
	if m == nil {
		return 640, 480
	}
	if m.FallbackW > 0 && m.FallbackH > 0 {
		return m.FallbackW, m.FallbackH
	}
	if m.CapW > 0 && m.CapH > 0 {
		return m.CapW, m.CapH
	}
	return 640, 480
}

// --- HIDSender (hotkey interception + routing to active target) ---

// SendText routes committed text to a backend that can inject Unicode. RDP
// falls back to ordinary HID keys when the selected device lacks this support.
func (b *Bridge) SendText(text string) error {
	m := b.activeMachine()
	if m == nil || m.HID == nil {
		return nil
	}
	if sender, ok := m.HID.(hid.TextSender); ok {
		return sender.SendText(text)
	}
	return hid.ErrTextUnsupported
}

func (b *Bridge) KeyDown(code uint8) error {
	if b.hotkey != nil {
		idx, matched := b.hotkey.CheckKeyDown(code)
		if matched {
			// Release any held modifier keys on the OLD target before switching.
			if m := b.activeMachine(); m != nil && m.HID != nil {
				m.HID.KeyUp()
			}
			b.SwitchMachine(idx)
			return nil
		}
	}
	m := b.activeMachine()
	if m == nil || m.HID == nil {
		return nil
	}
	return m.HID.KeyDown(code)
}

func (b *Bridge) KeyRelease(code uint8) error {
	if b.hotkey != nil && b.hotkey.CheckKeyRelease(code) {
		return nil
	}
	m := b.activeMachine()
	if m == nil || m.HID == nil {
		return nil
	}
	return m.HID.KeyRelease(code)
}

func (b *Bridge) KeyUp() error {
	m := b.activeMachine()
	if m == nil || m.HID == nil {
		return nil
	}
	return m.HID.KeyUp()
}

func (b *Bridge) MouseMove(dx, dy int) error {
	m := b.activeMachine()
	if m == nil || m.HID == nil {
		return nil
	}
	return m.HID.MouseMove(dx, dy)
}

func (b *Bridge) MouseMoveRel(dx, dy int) error {
	m := b.activeMachine()
	if m == nil || m.HID == nil {
		return nil
	}
	return m.HID.MouseMoveRel(dx, dy)
}

func (b *Bridge) MouseButton(button int, down bool) error {
	m := b.activeMachine()
	if m == nil || m.HID == nil {
		return nil
	}
	return m.HID.MouseButton(button, down)
}

func (b *Bridge) MouseWheel(delta int) error {
	m := b.activeMachine()
	if m == nil || m.HID == nil {
		return nil
	}
	m.wheelMu.Lock()
	defer m.wheelMu.Unlock()
	scaled := float64(delta)*m.WheelScale + m.wheelRemainder
	if math.IsNaN(scaled) {
		return nil
	}
	scaled = max(float64(math.MinInt32), min(float64(math.MaxInt32), scaled))
	if rounded := math.Round(scaled); math.Abs(scaled-rounded) < 1e-9 {
		scaled = rounded
	}
	steps := int(scaled)
	m.wheelRemainder = scaled - float64(steps)
	if steps == 0 {
		return nil
	}
	return m.HID.MouseWheel(steps)
}

// --- Client claim ---

func (b *Bridge) ClaimVNC() { b.claimSession("vnc") }

func (b *Bridge) ClaimWS() {
	b.claimSession("ws")
	b.needPixels.Store(false)
	b.devSetDecodeImage(false)
	b.SetVideoWantsH264(false)
	b.SetVideoWantsHEVC(false)
	b.SetVideoWantsAV1(false)
}

// StartWeb selects a target only after a browser has opened its WebSocket.
// The landing page can therefore browse configured targets without becoming a
// client or evicting an active VNC, RDP, or GameStream session.
func (b *Bridge) StartWeb(index int) error {
	if index < 0 || index >= len(b.machines) {
		return fmt.Errorf("invalid target index %d", index)
	}
	b.ClaimWS()
	b.SwitchMachine(index)
	return nil
}

func (b *Bridge) claimSession(proto string) {
	b.sessionMu.Lock()
	prev := b.activeProto
	b.activeProto = proto
	b.sessionMu.Unlock()

	if prev != "" && prev != proto {
		switch prev {
		case "viewer":
			b.viewerMu.RLock()
			viewer := b.viewerSrv
			b.viewerMu.RUnlock()
			if viewer != nil {
				viewer.CloseClients()
			}
		case "vnc":
			if b.vncSrv != nil {
				b.vncSrv.CloseClients()
			}
		case "ws":
			if b.wsSrv != nil {
				b.wsSrv.CloseClients()
			}
		case "gamestream":
			if b.gsSrv != nil {
				b.gsSrv.CloseClients()
			}
		case "rdp":
			if b.rdpSrv != nil {
				b.rdpSrv.CloseClients()
			}
		}
		log.Printf("bridge: %s client took over, kicked %s", proto, prev)
	}
	// A synchronous disconnect can reduce the old consumer count to zero
	// and clear ownership before the new protocol registers its consumer.
	b.sessionMu.Lock()
	if b.activeProto == "" {
		b.activeProto = proto
	}
	b.sessionMu.Unlock()

	if proto != "gamestream" && proto != "rdp" {
		b.captureMu.Lock()
		if b.desktopSize != nil {
			b.mu.Lock()
			capturing := b.capturing
			b.mu.Unlock()
			b.stopCaptureLocked()
			b.desktopSize = nil
			b.restoreDesktopsLocked()
			if capturing {
				b.startCaptureLocked()
			}
		}
		b.captureMu.Unlock()
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	switch proto {
	case "viewer":
		b.viewerMu.RLock()
		b.activeAudioSink = b.viewerAudioSink
		b.viewerMu.RUnlock()
	case "vnc":
		b.activeAudioSink = b.vncAudioSink
	case "ws":
		b.activeAudioSink = b.wsAudioSink
	case "gamestream":
		b.activeAudioSink = b.gsAudioSink
	case "rdp":
		b.activeAudioSink = b.rdpAudioSink
	default:
		b.activeAudioSink = nil
	}
}

func (b *Bridge) SetPixelNeed(need bool) {
	b.needPixels.Store(need)
	b.devSetDecodeImage(need)
	b.updateCodec()
}

func (b *Bridge) SetVideoWantsH264(want bool) {
	b.codecMu.Lock()
	b.wantH264 = want
	b.codecMu.Unlock()
	b.updateCodec()
}

func (b *Bridge) SetVideoCodecs(h264, hevc, av1 bool) {
	b.codecMu.Lock()
	b.wantH264, b.wantHEVC, b.wantAV1 = h264, hevc, av1
	b.codecMu.Unlock()
	b.updateCodec()
}

// RequestVideoKeyframe recovers browser reference loss without reopening capture.
func (b *Bridge) RequestVideoKeyframe() {
	b.captureMu.Lock()
	defer b.captureMu.Unlock()
	if dev, ok := b.activeDevice().(interface{ RequestKeyframe() error }); ok {
		if err := dev.RequestKeyframe(); err != nil {
			log.Printf("bridge: video keyframe request: %v", err)
		}
	}
}

func (b *Bridge) SetVideoWantsAV1(want bool) {
	b.codecMu.Lock()
	b.wantAV1 = want
	b.codecMu.Unlock()
	b.updateCodec()
}

func (b *Bridge) SetVideoWantsHEVC(want bool) {
	b.codecMu.Lock()
	b.wantHEVC = want
	b.codecMu.Unlock()
	b.updateCodec()
}

func (b *Bridge) devSetDecodeImage(v bool) {
	b.captureMu.Lock()
	defer b.captureMu.Unlock()
	b.devSetDecodeImageLocked(v)
}

func (b *Bridge) devSetDecodeImageLocked(v bool) {
	if dev := b.activeDevice(); dev != nil {
		dev.SetDecodeImage(v)
	}
}

type codecSwitcher interface {
	SetVideoCodec(string)
}

type chromaSwitcher interface{ SetVideoChroma(string) }
type codecCapable interface{ SupportsVideoCodec(string) bool }

func supportsGameStreamCodec(dev capture.Device, codec string) bool {
	if _, direct := dev.(capture.VideoStreamConfigurer); !direct {
		return true
	}
	if cc, ok := dev.(codecCapable); ok {
		if codec == "" {
			codec = "h264"
		}
		return cc.SupportsVideoCodec(codec)
	}
	return true
}

func (b *Bridge) updateCodec() {
	b.captureMu.Lock()
	defer b.captureMu.Unlock()
	b.updateCodecLocked()
}

func (b *Bridge) updateCodecLocked() {
	b.codecMu.Lock()
	defer b.codecMu.Unlock()

	dev := b.activeDevice()
	if dev == nil {
		return
	}

	b.mu.Lock()
	wantAV1 := b.wantAV1
	wantHEVC := b.wantHEVC
	wantH264 := b.wantH264
	needPixels := b.needPixels.Load()
	cur := b.videoCodec
	capturing := b.capturing
	b.mu.Unlock()
	b.sessionMu.Lock()
	wantNV12 := b.wantNV12 && b.activeProto == "rdp"
	b.sessionMu.Unlock()

	if b.gsVideo != nil {
		_, direct := dev.(capture.VideoStreamConfigurer)
		wantAV1 = direct && b.gsVideo.Codec == "av1"
		wantHEVC = direct && b.gsVideo.Codec == "hevc"
		wantH264, needPixels = direct && !wantHEVC && !wantAV1, !direct
		b.needPixels.Store(!direct)
	}
	vc, switchable := dev.(codecSwitcher)

	supports := func(codec string) bool {
		if cc, ok := dev.(codecCapable); ok {
			return cc.SupportsVideoCodec(codec)
		}
		return true
	}

	desired := ""
	switch {
	case wantAV1 && switchable && supports("av1") && supportsAV1Device(dev):
		desired = "av1"
	case wantHEVC && switchable && supports("hevc"):
		desired = "hevc"
	case wantH264 && switchable && supports("h264"):
		desired = "h264"
	case needPixels && switchable && wantNV12:
		// Use BGRA for the Planar converter unless the device advertises NV12.
		if cc, ok := dev.(codecCapable); ok && cc.SupportsVideoCodec("nv12") {
			desired = "nv12"
		} else if supports("rawvideo") {
			desired = "rawvideo"
		}
	case needPixels && switchable && supports("rawvideo"):
		desired = "rawvideo"
	}
	if desired == cur {
		return
	}

	if capturing {
		b.stopCaptureLocked()
	}
	if switchable {
		vc.SetVideoCodec(desired)
		// Chroma: VNC benefits from 4:2:2 (better text/UI edge quality);
		// the browser (WebCodecs) must stay 4:2:0.  MJPEG ignores this.
		chroma := "420"
		if desired == "h264" {
			b.sessionMu.Lock()
			proto := b.activeProto
			b.sessionMu.Unlock()
			if proto == "vnc" {
				chroma = "422"
			}
		}
		if cs, ok := dev.(chromaSwitcher); ok {
			cs.SetVideoChroma(chroma)
		}
	}
	b.mu.Lock()
	b.videoCodec = desired
	b.mu.Unlock()
	log.Printf("bridge: capture reconfigured (reopen=%v)", capturing)
	if capturing {
		b.startCaptureLocked()
	}
}

func (b *Bridge) Start() error {
	b.captureMu.Lock()
	defer b.captureMu.Unlock()
	b.mu.Lock()
	running := b.running
	b.mu.Unlock()
	if running {
		return nil
	}
	if b.vncSrv != nil {
		if err := b.vncSrv.Start(); err != nil {
			return err
		}
	}
	b.mu.Lock()
	b.running = true
	consumers := b.consumers
	b.mu.Unlock()
	if consumers > 0 {
		b.startCaptureLocked()
	}
	return nil
}

func (b *Bridge) Stop() {
	b.captureMu.Lock()
	defer b.captureMu.Unlock()
	b.mu.Lock()
	if !b.running {
		b.mu.Unlock()
		return
	}
	b.running = false
	b.setConsumersLocked(0)
	b.mu.Unlock()

	b.stopCaptureLocked()
	b.desktopSize = nil
	b.restoreDesktopsLocked()
	for _, m := range b.machines {
		if m.Capture != nil {
			m.Capture.Close()
		}
		stopTargetHID(m)
	}
	if b.vncSrv != nil {
		b.vncSrv.Close()
	}
}

func (b *Bridge) OnClientConnected() {
	b.captureMu.Lock()
	defer b.captureMu.Unlock()
	b.mu.Lock()
	b.setConsumersLocked(b.consumers + 1)
	n := b.consumers
	b.mu.Unlock()
	log.Printf("bridge: client connected (consumers=%d), ensuring capture", n)
	b.startCaptureLocked()
}

func (b *Bridge) OnClientDisconnected() {
	b.captureMu.Lock()
	defer b.captureMu.Unlock()
	b.mu.Lock()
	b.setConsumersLocked(b.consumers - 1)
	n := b.consumers
	b.mu.Unlock()
	log.Printf("bridge: client disconnected (consumers=%d)", n)

	if n > 0 {
		return
	}

	log.Printf("bridge: no consumers, stopping capture")
	stopTargetHID(b.activeMachine())
	b.stopCaptureLocked()
	b.desktopSize = nil
	b.restoreDesktopsLocked()

	// A new session can reserve its consumer while native restoration is in
	// progress and then wait for captureMu. Do not clear that owner's claim.
	// Hold mu through clearing ownership so a later reservation claims afresh.
	b.mu.Lock()
	if b.consumers > 0 {
		b.mu.Unlock()
		return
	}
	b.sessionMu.Lock()
	b.activeProto = ""
	b.sessionMu.Unlock()
	b.mu.Unlock()
	b.needPixels.Store(false)
	b.devSetDecodeImageLocked(false)

	b.codecMu.Lock()
	b.wantH264 = false
	b.wantHEVC = false
	b.wantAV1 = false
	b.wantNV12 = false
	b.codecMu.Unlock()
	b.mu.Lock()
	b.videoCodec = ""
	b.mu.Unlock()
	if dev := b.activeDevice(); dev != nil {
		if vc, ok := dev.(codecSwitcher); ok {
			vc.SetVideoCodec("")
		}
	}
}

func stopTargetHID(m *MachineSession) {
	if m == nil {
		return
	}
	m.wheelMu.Lock()
	m.wheelRemainder = 0
	m.wheelMu.Unlock()
	if session, ok := m.HID.(interface{ StopSession() error }); ok {
		if err := session.StopSession(); err != nil {
			log.Printf("bridge: target %q HID close: %v", m.Name, err)
		}
	}
}

func (b *Bridge) startCaptureLocked() {
	b.mu.Lock()
	controlled := b.running && b.consumers > 0
	b.mu.Unlock()
	if m := b.activeMachine(); controlled && m != nil {
		if session, ok := m.HID.(interface{ StartSession() error }); ok {
			if err := session.StartSession(); err != nil {
				log.Printf("bridge: target %q HID unavailable: %v", m.Name, err)
			}
		}
	}
	b.mu.Lock()

	if b.capturing || !b.running {
		b.mu.Unlock()
		return
	}

	m := b.activeMachineLocked()
	if m == nil {
		b.mu.Unlock()
		return
	}

	w, h := m.CapW, m.CapH
	fps := m.FPS
	if fps <= 0 {
		fps = b.fps
	}

	dev := m.Capture
	if dev == nil {
		log.Printf("bridge: no capture device for target %q", m.Name)
		b.pushFallbackLocked()
		b.mu.Unlock()
		return
	}
	// captureMu already serializes display changes and device open/close. Do
	// not hold the state mutex through native calls: HID lookups also use it,
	// and would otherwise block the RDP reader during a slow display switch.
	b.mu.Unlock()

	if surface, ok := dev.(capture.GPUSurfaceConfigurer); ok {
		b.sessionMu.Lock()
		rdp := b.activeProto == "rdp"
		b.sessionMu.Unlock()
		b.codecMu.Lock()
		want := b.wantSurface
		b.codecMu.Unlock()
		surface.SetGPUSurface(rdp && want)
	}
	if color, ok := dev.(capture.RDPAVC420Configurer); ok {
		b.sessionMu.Lock()
		rdp := b.activeProto == "rdp"
		b.sessionMu.Unlock()
		color.SetRDPAVC420(rdp && !b.needPixels.Load() && b.gsVideo == nil)
	}
	b.sessionMu.Lock()
	rdpOutput := b.activeProto == "rdp"
	b.sessionMu.Unlock()
	if policy, ok := dev.(capture.OutputSizePolicyConfigurer); ok {
		policy.SetOutputDownscaleOnly(rdpOutput)
	}
	if limit, ok := dev.(capture.OutputSizeLimitConfigurer); ok {
		mw, mh := 0, 0
		if rdpOutput {
			mw, mh = m.CapW, m.CapH
		}
		limit.SetOutputSizeLimit(mw, mh)
	}
	var outputRequest *capture.DesktopSize
	if b.desktopSize != nil {
		size := *b.desktopSize
		if rdpOutput && m.CapW > 0 && m.CapH > 0 {
			size.Width, size.Height = capture.BoundedOutputSize(m.CapW, m.CapH, size.Width, size.Height)
		}
		outputRequest = &size
	}
	if scaled, ok := dev.(capture.OutputSizeConfigurer); ok {
		ow, oh := 0, 0
		b.sessionMu.Lock()
		rdp := b.activeProto == "rdp"
		b.sessionMu.Unlock()
		if rdp && outputRequest != nil {
			ow, oh = outputRequest.Width, outputRequest.Height
		}
		scaled.SetOutputSize(ow, oh)
	}
	if desktop, ok := dev.(capture.DesktopConfigurer); ok {
		if m.AutoResolution && outputRequest != nil {
			request := *outputRequest
			if m.FPS > 0 {
				request.RefreshRate = m.FPS
			}
			_, err := desktop.SetDesktopSize(request)
			if err != nil {
				log.Printf("bridge: desktop resize for %q: %v", m.Name, err)
			}
			// Request this output canvas after the physical mode switch. RDP's
			// downscale-only policy bounds it to the actual mode on open.
			w, h = outputRequest.Width, outputRequest.Height
		}
		if target, ok := m.HID.(interface{ SetDisplayID(uint32) }); ok {
			if id := desktop.DesktopDisplayID(); id != 0 {
				target.SetDisplayID(id)
			}
		}
	}

	profile := b.gsVideo
	b.sessionMu.Lock()
	webJPEG := b.activeProto == "ws"
	b.sessionMu.Unlock()
	quality := 0
	if webJPEG {
		quality = b.webJPEGQuality
	}
	dev.SetJPEGQuality(quality)
	if native, ok := dev.(capture.EncoderBufferConfigurer); ok {
		frames := 0
		b.sessionMu.Lock()
		rdp := b.activeProto == "rdp"
		b.sessionMu.Unlock()
		if rdp && !b.needPixels.Load() {
			frames = b.rdpEncoderBufferFrames
		}
		native.SetEncoderBufferFrames(frames)
	}
	if native, ok := dev.(capture.VideoBitrateConfigurer); ok {
		kbps := 0
		auto := false
		b.sessionMu.Lock()
		rdp := b.activeProto == "rdp"
		b.sessionMu.Unlock()
		if rdp && !b.needPixels.Load() {
			kbps = b.rdpBitrate
			auto = kbps == 0
		}
		b.sessionMu.Lock()
		web := b.activeProto == "ws"
		vnc := b.activeProto == "vnc"
		b.sessionMu.Unlock()
		if web && !b.needPixels.Load() {
			kbps = b.webBitrate
		}
		if vnc && !b.needPixels.Load() {
			kbps = b.vncBitrate
		}

		if rateControl, ok := dev.(capture.VideoBitrateAutoConfigurer); ok {
			rateControl.SetVideoBitrateAuto(auto)
		}
		native.SetVideoBitrate(kbps)
	}
	if profile != nil && m.AutoResolution && m.FPS > 0 {
		limited := *profile
		limited.FPS = min(m.FPS, limited.FPS)
		profile = &limited
	}
	if direct, ok := dev.(capture.VideoStreamConfigurer); ok {
		direct.SetVideoStreamOptions(profile)
	}
	if scoped, ok := dev.(capture.GameStreamCaptureConfigurer); ok {
		scoped.SetGameStreamCaptureOptions(profile)
	}
	dev.SetDecodeImage(b.needPixels.Load())
	if err := dev.OpenWithFormat(w, h, fps); err != nil {
		log.Printf("bridge: failed to open device for %q: %v, using black placeholder", m.Name, err)
		b.restoreDesktopLocked(m)
		b.mu.Lock()
		b.pushFallbackLocked()
		b.mu.Unlock()
		return
	}
	log.Printf("bridge: capture device opened for %q, waiting for first frame", m.Name)
	b.mu.Lock()
	defer b.mu.Unlock()

	b.applyVideoFPSLocked()

	b.capturing = true
	run := &captureRun{stop: make(chan struct{}), width: w, height: h, fps: fps}
	b.run = run
	run.wg.Add(1)
	go b.captureLoop(m, run)

	// Audio starts if enabled for this target.
	b.startAudioLocked(m, run)
}

func (b *Bridge) applyVideoFPSLocked() {
	m := b.activeMachineLocked()
	if m == nil {
		return
	}
	fps := b.fps
	if fps <= 0 {
		if m.FPS > 0 {
			fps = m.FPS
		}
	}
	if fps <= 0 {
		if r, ok := m.Capture.(interface{ CaptureFPS() int }); ok {
			if f := r.CaptureFPS(); f > 0 {
				fps = f
			}
		}
	}
	if b.vncSrv != nil {
		b.vncSrv.SetFPS(fps)
		log.Printf("bridge: effective video fps = %d", fps)
	}
}

func (b *Bridge) startAudioLocked(m *MachineSession, run *captureRun) {
	if b.audioOn {
		return
	}
	if m == nil || m.AudioDev == nil {
		return
	}
	if b.activeAudioSink == nil {
		log.Printf("bridge: audio not started — active client has no audio sink")
		return
	}
	if err := m.AudioDev.Open(); err != nil {
		log.Printf("bridge: audio disabled for %q: %v", m.Name, err)
		return
	}
	b.audioOn = true
	log.Printf("bridge: audio capture started for %q", m.Name)
	run.wg.Add(1)
	go b.audioLoop(m, run)
}

func (b *Bridge) audioLoop(m *MachineSession, run *captureRun) {
	defer run.wg.Done()
	b.mu.Lock()
	initialSink := b.activeAudioSink
	b.mu.Unlock()
	defer setAudioAvailable(initialSink, false)
	forwarder := audioForwarder{}
	defer forwarder.close()
	var lastError time.Time
	var pushed, dropped uint64
	for {
		select {
		case <-run.stop:
			return
		default:
		}

		af, err := m.AudioDev.Capture()
		select {
		case <-run.stop:
			return // A device may finish a buffered read after Close.
		default:
		}
		if err != nil {
			if errors.Is(err, capture.ErrStreamStopped) {
				log.Printf("bridge: source audio stopped for %q", m.Name)
				return
			}
			log.Printf("bridge: audio capture error: %v", err)
			if !waitCapture(run.stop, 200*time.Millisecond) {
				return
			}
			continue
		}
		if af == nil || (len(af.PCM) == 0 && len(af.Encoded) == 0) {
			if status, ok := m.AudioDev.(interface{ AudioAvailable() bool }); ok && !status.AudioAvailable() {
				b.mu.Lock()
				sink := b.activeAudioSink
				b.mu.Unlock()
				setAudioAvailable(sink, false)
			}
			if !waitCapture(run.stop, 2*time.Millisecond) {
				return
			}
			continue
		}
		b.mu.Lock()
		sink := b.activeAudioSink
		b.mu.Unlock()
		if sink != nil {
			if err := forwarder.push(sink, *af); err != nil {
				setAudioAvailable(sink, false)
				if time.Since(lastError) >= 5*time.Second {
					log.Printf("bridge: audio decode: %v", err)
					lastError = time.Now()
				}
				continue
			}
			pushed++
			if pushed == 1 || pushed%250 == 0 {
				log.Printf("bridge: audio pushed %d chunks (%d bytes/chunk, source codec=%q)", pushed, len(af.PCM)+len(af.Encoded), af.Codec)
			}
		} else {
			dropped++
			if dropped == 1 {
				log.Printf("bridge: audio produced but no active sink (dropping)")
			}
		}
	}
}

func (b *Bridge) pushFallbackLocked() {
	m := b.activeMachineLocked()
	if m == nil {
		return
	}
	w, h := m.FallbackW, m.FallbackH
	if w <= 0 || h <= 0 {
		w, h = 640, 480
	}
	pixels := solidBGRA(w, h, 0, 0, 0)
	if b.needPixels.Load() {
		b.pushFrame(Frame{Format: capture.FormatRaw, Data: pixels, Width: w, Height: h,
			OutputMaxW: m.CapW, OutputMaxH: m.CapH}, nil)
	} else {
		data, err := jpegcodec.EncodeBGRA(pixels, w, h, 80)
		if err != nil {
			log.Printf("bridge: fallback JPEG: %v", err)
			return
		}
		b.pushFrame(Frame{Format: capture.FormatJPEG, Data: data, Width: w, Height: h,
			OutputMaxW: m.CapW, OutputMaxH: m.CapH}, nil)
	}
}

// Caller holds captureMu. Never wait for workers while holding mu: workers
// need it to finish their last iteration after Close unblocks device reads.
func (b *Bridge) stopCaptureLocked() {
	b.mu.Lock()
	run := b.run
	if run == nil {
		b.mu.Unlock()
		return
	}
	b.run = nil
	b.capturing = false
	audioOn := b.audioOn
	b.audioOn = false
	m := b.activeMachineLocked()
	close(run.stop)
	b.mu.Unlock()

	if m != nil {
		if m.Capture != nil {
			run.reopenMu.Lock()
			m.Capture.Close()
			run.reopenMu.Unlock()
		}
		if audioOn && m.AudioDev != nil {
			m.AudioDev.Close()
		}
	}
	run.wg.Wait()
}

func waitCapture(stop <-chan struct{}, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-stop:
		return false
	case <-timer.C:
		return true
	}
}

func (b *Bridge) push(format capture.VideoFormat, data []byte, width, height int, stop <-chan struct{}) {
	b.pushFrame(Frame{Format: format, Data: data, Width: width, Height: height}, stop)
}

func (b *Bridge) pushFrame(f Frame, stop <-chan struct{}) {
	if f.Surface != nil {
		// Surface capture is requested only for the exclusive Progressive GPU
		// session. Byte-oriented viewers must never receive empty raw frames.
		if b.rdpSrv != nil {
			if srv, ok := b.rdpSrv.(interface{ PushFrameWithStop(Frame, <-chan struct{}) }); ok {
				srv.PushFrameWithStop(f, stop)
			} else {
				b.rdpSrv.PushFrame(f)
			}
		}
		return
	}
	b.viewerMu.RLock()
	viewer := b.viewerSrv
	b.viewerMu.RUnlock()
	if viewer != nil {
		viewer.PushFrame(f)
	}
	if b.rdpSrv != nil {
		if srv, ok := b.rdpSrv.(interface{ PushFrameWithStop(Frame, <-chan struct{}) }); ok {
			srv.PushFrameWithStop(f, stop)
		} else {
			b.rdpSrv.PushFrame(f)
		}
	}
	format, data, width, height := f.Format, f.Data, f.Width, f.Height
	if b.vncSrv != nil {
		b.vncSrv.PushFrameWithStop(vnc.Frame{Timing: f.Timing, Buffer: f.Buffer, Format: format, Data: data, Width: width, Height: height}, stop)
	}
	if b.gsSrv != nil {
		if srv, ok := b.gsSrv.(interface{ PushFrameWithStop(Frame, <-chan struct{}) }); ok {
			srv.PushFrameWithStop(f, stop)
		} else {
			b.gsSrv.PushFrame(f)
		}
	}
	if srv, ok := b.wsSrv.(interface{ PushFrameWithStop(Frame, <-chan struct{}) }); ok {
		srv.PushFrameWithStop(f, stop)
	} else if b.wsSrv != nil {
		b.wsSrv.PushFrame(f)
	}
}

func (b *Bridge) captureLoop(m *MachineSession, run *captureRun) {
	defer run.wg.Done()
	firstFrame := true
	idle := captureIdleMonitor{lastFrame: time.Now()}
	recovery := captureRecovery{}
	statsStart := time.Now()
	var maxRead, maxPush time.Duration
	frames := 0
	for {
		select {
		case <-run.stop:
			return
		default:
		}

		readStart := time.Now()
		frame, err := m.Capture.Capture()
		maxRead = max(maxRead, time.Since(readStart))
		if err != nil {
			frame.Release()
			select {
			case <-run.stop:
				return
			default:
			}
			if errors.Is(err, capture.ErrStreamStopped) {
				if source, ok := m.Capture.(interface{ CanRecoverCapture() bool }); ok && source.CanRecoverCapture() {
					if !recoverCapture(m.Capture, run, &recovery, err.Error()) {
						return
					}
					firstFrame = true
					idle.lastFrame = time.Now()
					continue
				}
				log.Printf("bridge: capture terminated: %v; closing session, reconnect to retry", err)
				// Cleanup waits for this worker, so run it after we return.
				go b.closeFailedCapture(run)
				return
			}
			log.Printf("bridge: capture error: %v", err)
			if !waitCapture(run.stop, time.Second) {
				return
			}
			continue
		}
		if frame == nil || (len(frame.Data) == 0 && frame.Surface == nil) {
			frame.Release()
			now := time.Now()
			if idle.due(now) {
				expected, detail := false, ""
				if source, ok := m.Capture.(interface{ CaptureIdleStatus() (bool, string) }); ok {
					expected, detail = source.CaptureIdleStatus()
				}
				if firstFrame || !expected {
					log.Printf("bridge: no captured frame for %.1fs (waiting_first=%v) %s", now.Sub(idle.lastFrame).Seconds(), firstFrame, detail)
				}
				idle.lastCheck = now
			}
			if idle.recoveryDue(now) {
				if source, ok := m.Capture.(interface {
					CanRecoverCapture() bool
					CaptureIdleStatus() (bool, string)
				}); ok && source.CanRecoverCapture() {
					expected, detail := source.CaptureIdleStatus()
					if firstFrame || !expected {
						if !recoverCapture(m.Capture, run, &recovery, detail) {
							return
						}
						firstFrame = true
						idle.lastFrame = time.Now()
						continue
					}
				}
			}
			if !waitCaptureFrame(m.Capture, run.stop) {
				return
			}
			continue
		}
		idle.lastFrame = time.Now()
		recovery.frame(idle.lastFrame)
		b.mu.Lock()
		if firstFrame || frame.Width != m.FallbackW || frame.Height != m.FallbackH {
			log.Printf("bridge: first captured frame %dx%d (was %dx%d)",
				frame.Width, frame.Height, m.CapW, m.CapH)
			m.FallbackW = frame.Width
			m.FallbackH = frame.Height
			if b.vncSrv != nil {
				b.vncSrv.UpdateFramebufferSize(frame.Width, frame.Height)
			}
			if b.wsSrv != nil {
				b.wsSrv.UpdateFramebufferSize(frame.Width, frame.Height)
			}
			firstFrame = false
		}
		b.mu.Unlock()

		select {
		case <-run.stop:
			frame.Release()
			return
		default:
		}
		pushStart := time.Now()
		preserve := false
		if source, ok := m.Capture.(interface{ PreserveCaptureMode() bool }); ok {
			preserve = source.PreserveCaptureMode()
		}
		sourceFPS := m.FPS
		if source, ok := m.Capture.(interface{ CaptureFPS() int }); ok && source.CaptureFPS() > 0 {
			sourceFPS = source.CaptureFPS()
		}
		sourceRate := framerate.New(sourceFPS, 1)
		if source, ok := m.Capture.(interface{ CaptureFrameRate() framerate.Rate }); ok {
			sourceRate = source.CaptureFrameRate()
		}
		_, scalesOutput := m.Capture.(capture.OutputSizeConfigurer)
		// Fixed sources may pass compressed input through unchanged, or fit
		// raw input before encoding. Their actual AU size is authoritative.
		scalesOutput = scalesOutput && !preserve
		bufferH264 := false
		if source, ok := m.Capture.(interface{ BufferH264Presentation() bool }); ok && frame.Format == capture.FormatH264 {
			bufferH264 = source.BufferH264Presentation()
		}
		b.pushFrame(Frame{Surface: frame.Surface, Timing: videotiming.Delivery{Trace: frame.Timing}, BufferH264Presentation: bufferH264, PreserveSource: preserve, CaptureScalesOutput: scalesOutput, OutputMaxW: m.CapW, OutputMaxH: m.CapH, CaptureFPS: sourceFPS, CaptureRate: sourceRate, Buffer: frame.Buffer, CapturedAt: frame.CapturedAt, Format: frame.Format, PixFmt: frame.PixFmt, Data: frame.Data, Width: frame.Width, Height: frame.Height, Rotation: frame.Rotation, SourceWidth: frame.SourceWidth, SourceHeight: frame.SourceHeight, ContentRect: frame.ContentRect}, run.stop)
		frame.Release()
		maxPush = max(maxPush, time.Since(pushStart))
		frames++
		if elapsed := time.Since(statsStart); elapsed >= 5*time.Second {
			log.Printf("bridge: video %.1f fps max_read=%.1fms max_push=%.1fms", float64(frames)/elapsed.Seconds(), float64(maxRead)/float64(time.Millisecond), float64(maxPush)/float64(time.Millisecond))
			statsStart, frames, maxRead, maxPush = time.Now(), 0, 0, 0
		}
	}
}

func solidBGRA(w, h int, r, g, b uint8) []byte {
	pixels := make([]byte, w*h*4)
	for i := 0; i < len(pixels); i += 4 {
		pixels[i] = b
		pixels[i+1] = g
		pixels[i+2] = r
		pixels[i+3] = 255
	}
	return pixels
}

// Serialized with client disconnects and source switches. A delayed failure
// from an old run must not close a newly opened capture session.
func (b *Bridge) closeFailedCapture(failed *captureRun) {
	b.captureMu.Lock()
	defer b.captureMu.Unlock()
	b.mu.Lock()
	current := b.run == failed
	b.mu.Unlock()
	if current {
		b.stopCaptureLocked()
		b.restoreDesktopsLocked()
	}
}

// SupportsGameStreamHEVC uses the source or driver codec declaration.
func (b *Bridge) SupportsGameStreamHEVC(index int) bool {
	if index < 0 || index >= len(b.machines) {
		return false
	}
	dev, ok := b.machines[index].Capture.(interface{ SupportsGameStreamHEVC() bool })
	return ok && dev.SupportsGameStreamHEVC()
}

func supportsAV1Device(dev capture.Device) bool {
	capable, ok := dev.(interface{ SupportsGameStreamAV1() bool })
	return ok && capable.SupportsGameStreamAV1()
}
func (b *Bridge) SupportsGameStreamAV1(index int) bool {
	return index >= 0 && index < len(b.machines) && supportsAV1Device(b.machines[index].Capture)
}

// Retain returns an independent reference suitable for an asynchronous queue.
// Legacy reusable source data is copied once on entry to an owned queue.
func (f Frame) Retain() Frame {
	if f.Surface != nil && f.Buffer == nil {
		panic("bridge: GPU surface has no lifetime owner")
	}
	if f.Buffer != nil {
		f.Buffer = f.Buffer.Retain()
	} else {
		f.Buffer = framebuf.Acquire(f.Data, nil)
		f.Data = f.Buffer.Bytes()
	}
	return f
}
func (f *Frame) Release() {
	if f != nil {
		f.Buffer.Release()
		f.Buffer = nil
		f.Data = nil
		f.Surface = nil
	}
}
