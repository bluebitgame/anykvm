//go:build viewer

package viewer

import (
	"log"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"anykvm/internal/capture"
	"anykvm/internal/jpegcodec"

	"github.com/veandco/go-sdl2/sdl"
)

type InputSender interface {
	KeyDown(keycode uint8) error
	KeyUp() error
	KeyRelease(keycode uint8) error
	MouseMove(dx, dy int) error
	MouseMoveRel(dx, dy int) error
	MouseButton(button int, down bool) error
	MouseWheel(delta int) error
}

type GUI struct {
	window   *sdl.Window
	renderer *sdl.Renderer
	texture  *sdl.Texture

	dev    capture.Device
	input  InputSender
	ffPath string

	capW        int
	capH        int
	mouseMode   string
	scrollMode  ScrollMode
	wheelInvert bool
	yuvPixFmt   string

	running       atomic.Bool
	stopRequested atomic.Bool
	pressed       map[sdl.Keycode]bool

	decoder *decoder
	decFmt  capture.VideoFormat
	decMu   sync.Mutex

	lastX      int
	lastY      int
	frameCount uint64
	lastFPS    time.Time

	needClear bool
	lastWW    int32
	lastWH    int32

	audioRing *audioRing
	audioDev  sdl.AudioDeviceID

	gcMouseLastCount int // darwin: last reported GCMouse count (for transition logs)
	gcResyncAt       time.Time
	windowFocused    bool
}

type audioRing struct {
	buf []byte
	sz  int
	rp  int
	wp  int
	mu  sync.Mutex
}

func newAudioRing(size int) *audioRing {
	return &audioRing{buf: make([]byte, size), sz: size}
}

func (a *audioRing) avail() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.wp >= a.rp {
		return a.wp - a.rp
	}
	return a.sz - a.rp + a.wp
}

func (a *audioRing) write(data []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, b := range data {
		next := a.wp + 1
		if next >= a.sz {
			next = 0
		}
		if next == a.rp {
			a.rp++
			if a.rp >= a.sz {
				a.rp = 0
			}
		}
		a.buf[a.wp] = b
		a.wp = next
	}
}

func (a *audioRing) read(n int) []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		if a.rp == a.wp {
			out[i] = 0
		} else {
			out[i] = a.buf[a.rp]
			a.rp++
			if a.rp >= a.sz {
				a.rp = 0
			}
		}
	}
	return out
}

func New(dev capture.Device, input InputSender, ffPath, mouseMode string, scrollMode ScrollMode) *GUI {
	if mouseMode != "relative" {
		mouseMode = "absolute"
	}
	if scrollMode == "" {
		scrollMode = ScrollNone
	}
	return &GUI{
		dev:         dev,
		input:       input,
		ffPath:      ffPath,
		mouseMode:   mouseMode,
		scrollMode:  scrollMode,
		wheelInvert: wheelInvertForMode(scrollMode),
		pressed:     make(map[sdl.Keycode]bool),
		needClear:   true,
	}
}

// Stop requests a clean exit from any goroutine.
func (g *GUI) Stop() { g.stopRequested.Store(true); g.running.Store(false) }

func (g *GUI) Run() error {
	if err := sdl.Init(sdl.INIT_VIDEO | sdl.INIT_EVENTS | sdl.INIT_AUDIO); err != nil {
		return err
	}
	defer sdl.Quit()

	window, err := sdl.CreateWindow("AnyKVM", sdl.WINDOWPOS_UNDEFINED, sdl.WINDOWPOS_UNDEFINED,
		1280, 720, sdl.WINDOW_RESIZABLE|sdl.WINDOW_ALLOW_HIGHDPI)
	if err != nil {
		return err
	}
	g.window = window
	defer window.Destroy()

	renderer, err := sdl.CreateRenderer(window, -1, sdl.RENDERER_ACCELERATED|sdl.RENDERER_PRESENTVSYNC)
	if err != nil {
		return err
	}
	g.renderer = renderer
	defer renderer.Destroy()

	renderer.SetDrawColor(0, 0, 0, 255)

	g.initAudio()

	if cs, ok := g.dev.(interface{ SetVideoCodec(string) }); ok {
		cs.SetVideoCodec("uyvy")
	}

	if err := g.dev.OpenWithFormat(0, 0, 30); err != nil {
		return err
	}

	g.running.Store(true)
	g.lastFPS = time.Now()

	_, dynamicMouseMode := g.input.(interface{ MouseMode() string })
	if g.mouseMode == "relative" || dynamicMouseMode {
		platformStartRawMouse()
		mouseWake := make(chan struct{}, 1)
		mouseStop := make(chan struct{})
		platformStartGCMouseRelay(mouseWake)
		go g.gcMouseLoop(mouseWake, mouseStop)
		defer func() {
			close(mouseStop)
			platformStopGCMouseRelay()
			platformStopRawMouse()
		}()
		g.updateGCMouseStatus(true)
		g.windowFocused = (g.window.GetFlags() & sdl.WINDOW_INPUT_FOCUS) != 0
		platformSetRawMouseActive(g.windowFocused && g.mouseMode == "relative")
		if g.mouseMode == "relative" {
			g.enableRelativeMouse()
		}
		defer sdl.SetRelativeMouseMode(false)
	}

	frameReady := make(chan *capture.Frame, 1)
	renderDone := make(chan struct{}, 1)
	captureDone := make(chan struct{})
	go g.captureLoop(frameReady, renderDone, captureDone)
	defer func() {
		g.running.Store(false)
		g.dev.Close() // interrupt a Capture blocked waiting for the next frame
		<-captureDone
		for {
			select {
			case f := <-frameReady:
				f.Release()
			default:
				return
			}
		}
	}()

	// ~125Hz SDL event wakeups between frames; GCMouse forwards on callback (no poll drain).
	poll := time.NewTicker(8 * time.Millisecond)
	defer poll.Stop()

	for g.running.Load() && !g.stopRequested.Load() {
		g.handleEvents()
		if g.mouseMode == "relative" {
			g.maybeResyncGCMouse()
		}

		select {
		case frame := <-frameReady:
			g.renderFrame(frame)
			g.frameCount++
			g.updateFPS()
			renderDone <- struct{}{}
			g.handleEvents()
		case <-poll.C:
		}
	}

	// Unblock captureLoop if it is waiting for renderDone.
	select {
	case renderDone <- struct{}{}:
	default:
	}

	if g.decoder != nil {
		g.decoder.close()
	}
	return nil
}

// captureLoop runs in a background goroutine so Capture() blocking does not stall
// SDL input polling on the main thread. Each frame is handed off only after the
// main thread finishes rendering the previous one, avoiding capture buffer reuse.
func (g *GUI) captureLoop(frameReady chan<- *capture.Frame, renderDone <-chan struct{}, done chan<- struct{}) {
	defer close(done)

	for g.running.Load() && !g.stopRequested.Load() {
		var frame *capture.Frame
		for g.running.Load() && !g.stopRequested.Load() {
			f, err := g.dev.Capture()
			if err != nil {
				f.Release()
				log.Printf("gui: capture error: %v", err)
				time.Sleep(time.Second)
				continue
			}
			if f != nil && len(f.Data) > 0 {
				frame = f
				break
			}
			f.Release()
			time.Sleep(2 * time.Millisecond)
		}
		if !g.running.Load() || frame == nil {
			frame.Release()
			return
		}

		select {
		case frameReady <- frame:
		case <-time.After(100 * time.Millisecond):
			frame.Release()
			if !g.running.Load() {
				return
			}
			continue
		}
		if !g.running.Load() {
			return
		}

		select {
		case <-renderDone:
		case <-time.After(100 * time.Millisecond):
			if !g.running.Load() {
				return
			}
		}
	}
}

// Follow the active target after a switch, including absolute/relative input.
func (g *GUI) updateMouseMode() {
	source, ok := g.input.(interface{ MouseMode() string })
	if !ok {
		return
	}
	mode := source.MouseMode()
	if mode == g.mouseMode {
		return
	}
	g.mouseMode = mode
	platformSetRawMouseActive(g.windowFocused && mode == "relative")
	if mode == "relative" {
		g.enableRelativeMouse()
	} else {
		sdl.SetRelativeMouseMode(false)
	}
}

func (g *GUI) handleEvents() {
	g.updateMouseMode()
	for event := sdl.PollEvent(); event != nil; event = sdl.PollEvent() {
		g.updateMouseMode()
		switch e := event.(type) {
		case *sdl.QuitEvent:
			g.running.Store(false)
		case *sdl.KeyboardEvent:
			g.handleKeyboard(e)
		case *sdl.MouseMotionEvent:
			g.handleMouseMotion(e)
		case *sdl.WindowEvent:
			switch e.Event {
			case sdl.WINDOWEVENT_FOCUS_GAINED:
				g.windowFocused = true
				platformSetRawMouseActive(true)
				if g.mouseMode == "relative" {
					g.updateGCMouseStatus(true)
					g.enableRelativeMouse()
				}
			case sdl.WINDOWEVENT_FOCUS_LOST:
				g.windowFocused = false
				platformSetRawMouseActive(false)
			}
		case *sdl.MouseButtonEvent:
			g.handleMouseButton(e)
		case *sdl.MouseWheelEvent:
			g.handleMouseWheel(e)
		}
	}
}

func (g *GUI) handleKeyboard(e *sdl.KeyboardEvent) {
	if g.input == nil {
		return
	}
	usage, ok := hidForKey(e.Keysym.Sym)
	if !ok {
		return
	}
	if e.Type == sdl.KEYDOWN && e.Repeat == 0 {
		if !g.pressed[e.Keysym.Sym] {
			g.pressed[e.Keysym.Sym] = true
			g.input.KeyDown(usage)
		}
	} else if e.Type == sdl.KEYUP {
		delete(g.pressed, e.Keysym.Sym)
		g.input.KeyRelease(usage)
	}
}

func (g *GUI) enableRelativeMouse() {
	if ret := sdl.SetRelativeMouseMode(true); ret != 0 {
		log.Printf("gui: relative mouse capture failed: %s", sdl.GetError())
	}
}

func (g *GUI) handleMouseMotion(e *sdl.MouseMotionEvent) {
	if g.input == nil {
		return
	}
	if g.mouseMode == "relative" {
		if platformGCMouseCount() > 0 {
			return
		}
		g.forwardMouseRel(int(e.XRel), int(e.YRel), "sdl")
		return
	}
	ww, wh := g.window.GetSize()
	ax, ay, ok := g.mapWindowToHID(int32(e.X), int32(e.Y), ww, wh)
	if !ok {
		return
	}
	if ax != g.lastX || ay != g.lastY {
		g.input.MouseMove(ax, ay)
	}
	g.lastX, g.lastY = ax, ay
}

// gcMouseLoop runs on a dedicated goroutine: wake on GCMouse delta, coalesce
// accumulated motion, then send one HID packet (avoids per-event serial writes).
func (g *GUI) gcMouseLoop(wake <-chan struct{}, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-wake:
		}
		for {
			var totalX, totalY float32
			drained := false
			for {
				dx, dy, ok := platformDrainRawMouse()
				if !ok {
					break
				}
				drained = true
				totalX += dx
				totalY += dy
			}
			if drained && platformGCMouseCount() > 0 && g.input != nil {
				g.forwardMouseRel(int(totalX), int(totalY), "gc")
			}
			select {
			case <-wake:
				continue
			default:
			}
			break
		}
	}
}

func (g *GUI) maybeResyncGCMouse() {
	now := time.Now()
	if !g.gcResyncAt.IsZero() && now.Before(g.gcResyncAt) {
		return
	}
	g.gcResyncAt = now.Add(500 * time.Millisecond)
	g.updateGCMouseStatus(false)
}

func (g *GUI) updateGCMouseStatus(forceLog bool) {
	platformResyncRawMouse()
	n := platformGCMouseCount()
	if !forceLog && n == g.gcMouseLastCount {
		return
	}
	g.gcMouseLastCount = n
	if n > 0 {
		log.Printf("gui: GCMouse raw input (%d device(s), using current); SDL motion ignored while active", n)
		return
	}
	log.Printf("gui: no GCMouse devices — SDL relative motion (trackpad; Mac acceleration applies)")
}

func (g *GUI) forwardMouseRel(dx, dy int, source string) {
	if dx == 0 && dy == 0 {
		return
	}
	if dbg, ok := g.input.(*debugInput); ok {
		dbg.logMouseRel(source, dx, dy)
		if err := dbg.inner.MouseMoveRel(dx, dy); err != nil {
			log.Printf("gui: mouse rel: %v", err)
		}
		return
	}
	if err := g.input.MouseMoveRel(dx, dy); err != nil {
		log.Printf("gui: mouse rel: %v", err)
	}
}

// mapWindowToHID maps SDL window coordinates to USB HID absolute range
// (0–32767), accounting for letterboxed video within the window.
func (g *GUI) mapWindowToHID(mx, my, ww, wh int32) (ax, ay int, ok bool) {
	if ww <= 0 || wh <= 0 {
		return 0, 0, false
	}
	dst := g.letterbox(ww, wh)
	var lx, ly, vw, vh int32
	if dst == nil {
		lx, ly, vw, vh = mx, my, ww, wh
	} else {
		lx = mx - dst.X
		ly = my - dst.Y
		vw, vh = dst.W, dst.H
	}
	if vw <= 0 || vh <= 0 {
		return 0, 0, false
	}
	if lx < 0 {
		lx = 0
	} else if lx >= vw {
		lx = vw - 1
	}
	if ly < 0 {
		ly = 0
	} else if ly >= vh {
		ly = vh - 1
	}
	ax = int(lx * 32767 / vw)
	ay = int(ly * 32767 / vh)
	if ax < 0 {
		ax = 0
	} else if ax > 32767 {
		ax = 32767
	}
	if ay < 0 {
		ay = 0
	} else if ay > 32767 {
		ay = 32767
	}
	return ax, ay, true
}

func (g *GUI) handleMouseButton(e *sdl.MouseButtonEvent) {
	if g.input == nil {
		return
	}
	btn := 0
	switch e.Button {
	case sdl.BUTTON_LEFT:
		btn = 1
	case sdl.BUTTON_MIDDLE:
		btn = 3
	case sdl.BUTTON_RIGHT:
		btn = 2
	default:
		return
	}
	down := e.Type == sdl.MOUSEBUTTONDOWN
	g.input.MouseButton(btn, down)
}

func (g *GUI) handleMouseWheel(e *sdl.MouseWheelEvent) {
	if g.input == nil {
		return
	}
	delta := wheelDeltaFromSDL(e.Y, g.wheelInvert)
	if delta == 0 {
		return
	}
	g.input.MouseWheel(delta)
}

func (g *GUI) renderFrame(frame *capture.Frame) {
	defer frame.Release()
	if g.capW == 0 || g.capH == 0 {
		g.capW = frame.Width
		g.capH = frame.Height
		if frame.Format == capture.FormatYUV {
			g.yuvPixFmt = frame.PixFmt
		}
		log.Printf("gui: capture resolution %dx%d pixfmt=%q datalen=%d",
			g.capW, g.capH, frame.PixFmt, len(frame.Data))
		if g.capW > 0 && g.capH > 0 {
			g.recreateTexture()
			g.fitWindow()
			g.needClear = true
		}
	}

	if frame.Width != g.capW || frame.Height != g.capH {
		if frame.Format == capture.FormatYUV {
			g.yuvPixFmt = frame.PixFmt
		}
		log.Printf("gui: resolution changed %dx%d -> %dx%d", g.capW, g.capH, frame.Width, frame.Height)
		g.capW = frame.Width
		g.capH = frame.Height
		g.recreateTexture()
		g.fitWindow()
		g.needClear = true
	}

	if !g.extractPixels(frame) {
		return
	}

	if g.texture == nil {
		return
	}
	ww, wh, _ := g.renderer.GetOutputSize()
	if ww != g.lastWW || wh != g.lastWH {
		g.needClear = true
		g.lastWW = ww
		g.lastWH = wh
	}
	if g.needClear {
		g.renderer.Clear()
		g.needClear = false
	}
	dst := g.letterbox(ww, wh)
	g.renderer.Copy(g.texture, nil, dst)
	g.renderer.Present()
}

func (g *GUI) extractPixels(frame *capture.Frame) bool {
	switch frame.Format {
	case capture.FormatRaw:
		if len(frame.Data) == g.capW*g.capH*4 {
			g.updateTexture(frame.Data)
			return true
		}
	case capture.FormatYUV:
		if g.yuvPixFmt == "nv12" && len(frame.Data) == g.capW*g.capH*3/2 {
			g.updateNV12Texture(frame.Data)
			return true
		}
		if len(frame.Data) == g.capW*g.capH*2 {
			g.updateTexture(frame.Data)
			return true
		}
	}

	data, err := g.decodeFrame(frame)
	if err != nil {
		log.Printf("gui: decode error: %v", err)
		return false
	}
	g.updateTexture(data)
	return true
}

func (g *GUI) decodeFrame(frame *capture.Frame) ([]byte, error) {
	switch frame.Format {
	case capture.FormatRaw:
		return frame.Data, nil
	case capture.FormatYUV:
		return frame.Data, nil
	case capture.FormatJPEG:
		pixels, _, _, err := jpegcodec.DecodeBGRA(frame.Data)
		return pixels, err
	case capture.FormatH264, capture.FormatHEVC:
		return g.decodeWithPipe(frame)
	default:
		return nil, nil
	}
}

func (g *GUI) decodeWithPipe(frame *capture.Frame) ([]byte, error) {
	g.decMu.Lock()
	defer g.decMu.Unlock()

	if g.decoder == nil || g.decFmt != frame.Format {
		if g.decoder != nil {
			g.decoder.close()
		}
		d, err := newDecoder(g.ffPath, frame.Width, frame.Height, frame.Format)
		if err != nil {
			g.decoder = nil
			return nil, err
		}
		g.decoder = d
		g.decFmt = frame.Format
	}

	return g.decoder.decode(frame.Data)
}

func (g *GUI) recreateTexture() {
	if g.texture != nil {
		g.texture.Destroy()
		g.texture = nil
	}

	pf := uint32(sdl.PIXELFORMAT_BGRA32)
	switch g.yuvPixFmt {
	case "uyvy422":
		pf = uint32(sdl.PIXELFORMAT_UYVY)
	case "yuyv422":
		pf = uint32(sdl.PIXELFORMAT_YUY2)
	case "nv12":
		pf = 0x3231564E // SDL_PIXELFORMAT_NV12 (not exported by go-sdl2)
	}

	tex, err := g.renderer.CreateTexture(pf, sdl.TEXTUREACCESS_STREAMING, int32(g.capW), int32(g.capH))
	if err != nil {
		log.Printf("gui: create texture error: %v (falling back to BGRA)", err)
		g.yuvPixFmt = ""
		tex, err = g.renderer.CreateTexture(uint32(sdl.PIXELFORMAT_BGRA32), sdl.TEXTUREACCESS_STREAMING, int32(g.capW), int32(g.capH))
		if err != nil {
			log.Printf("gui: create texture error: %v", err)
			return
		}
	}
	g.texture = tex
}

func (g *GUI) updateTexture(pixels []byte) {
	if g.texture == nil {
		return
	}
	pitch := g.capW * 4
	if g.yuvPixFmt == "uyvy422" || g.yuvPixFmt == "yuyv422" {
		pitch = g.capW * 2
	}
	g.texture.Update(nil, unsafe.Pointer(&pixels[0]), pitch)
}

func (g *GUI) updateNV12Texture(data []byte) {
	if g.texture == nil {
		return
	}
	yPlane := data[:g.capW*g.capH]
	uvPlane := data[g.capW*g.capH:]
	g.texture.UpdateNV(nil, yPlane, g.capW, uvPlane, g.capW)
}

func (g *GUI) fitWindow() {
	di, err := g.window.GetDisplayIndex()
	if err != nil {
		g.window.SetSize(int32(g.capW), int32(g.capH))
		return
	}
	bounds, err := sdl.GetDisplayBounds(di)
	if err != nil {
		g.window.SetSize(int32(g.capW), int32(g.capH))
		return
	}

	ratio := g.pixelRatio()
	w := int32(float64(g.capW) / ratio)
	h := int32(float64(g.capH) / ratio)
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}

	if w > bounds.W || h > bounds.H {
		scaleW := float64(bounds.W) / float64(w)
		scaleH := float64(bounds.H) / float64(h)
		scale := scaleW
		if scaleH < scale {
			scale = scaleH
		}
		if scale < 0.1 {
			scale = 0.1
		}
		w = int32(float64(w) * scale)
		h = int32(float64(h) * scale)
	}

	g.window.SetSize(w, h)
}

func (g *GUI) pixelRatio() float64 {
	if g.renderer == nil {
		return 1.0
	}
	ww, wh, err := g.renderer.GetOutputSize()
	if err != nil || ww <= 0 || wh <= 0 {
		return 1.0
	}
	lw, lh := g.window.GetSize()
	if lw <= 0 || lh <= 0 {
		return 1.0
	}
	rw := float64(ww) / float64(lw)
	rh := float64(wh) / float64(lh)
	if rw > rh {
		return rw
	}
	return rh
}

func (g *GUI) letterbox(ww, wh int32) *sdl.Rect {
	if g.capW <= 0 || g.capH <= 0 || ww <= 0 || wh <= 0 {
		return nil
	}
	cw := float64(g.capW)
	ch := float64(g.capH)
	s := float64(ww) / cw
	if float64(wh)/ch < s {
		s = float64(wh) / ch
	}
	dw := int32(cw * s)
	dh := int32(ch * s)
	return &sdl.Rect{
		X: (ww - dw) / 2,
		Y: (wh - dh) / 2,
		W: dw,
		H: dh,
	}
}

func (g *GUI) updateFPS() {
	now := time.Now()
	elapsed := now.Sub(g.lastFPS)
	if elapsed >= time.Second {
		fps := float64(g.frameCount) / elapsed.Seconds()
		g.frameCount = 0
		g.lastFPS = now
		g.window.SetTitle(formatTitle(fps, g.capW, g.capH))
	}
}

func (g *GUI) initAudio() {
	g.audioRing = newAudioRing(192000)

	spec := &sdl.AudioSpec{
		Freq:     48000,
		Format:   sdl.AUDIO_S16LSB,
		Channels: 2,
		Samples:  2048,
	}

	audioDev, err := sdl.OpenAudioDevice("", false, spec, nil, 0)
	if err != nil {
		log.Printf("gui: audio init error: %v", err)
		return
	}
	g.audioDev = audioDev
	sdl.PauseAudioDevice(audioDev, false)

	go func() {
		for {
			sdl.Delay(10)
			if !g.running.Load() {
				break
			}
			if g.audioRing.avail() >= 4096 {
				data := g.audioRing.read(4096)
				sdl.QueueAudio(audioDev, data)
			}
		}
	}()
}

func (g *GUI) AudioCallback() func([]byte) {
	return func(pcm []byte) {
		g.audioRing.write(pcm)
	}
}

func formatTitle(fps float64, w, h int) string {
	return "AnyKVM"
}
