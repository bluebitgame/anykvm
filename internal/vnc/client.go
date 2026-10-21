package vnc

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"anykvm/internal/capture"
	"anykvm/internal/framebuf"
	"anykvm/internal/hid"
	"anykvm/internal/jpegcodec"
	"anykvm/internal/pixelops"
	"anykvm/internal/videotiming"
)

type client struct {
	codecs *CodecOptions // immutable policy captured when accepted
	conn   net.Conn
	server *Server

	width  uint16
	height uint16

	encodings   []int32
	pixelFmt    pixelFormat // negotiated pixel format (default BGR888/32bpp)
	jpegQuality int         // desired JPEG quality (1-100), 0 = not set
	jpegLock    int         // forced JPEG quality (1-100), 0 = not locked
	encMu       sync.Mutex  // guards encodings, pixelFmt and jpegQuality
	incremental bool
	reqX, reqY  uint16
	reqW, reqH  uint16

	// Frame delivery model (guarded by stateMu):
	//   - Default: request-driven. A FramebufferUpdateRequest arms pendingReq;
	//     one frame is sent per request.
	//   - continuousUpdates: the client enabled EnableContinuousUpdates; frames
	//     are pushed proactively without per-frame requests.
	stateMu          sync.Mutex
	pendingReq       bool       // a framebuffer update has been requested and not yet served
	contUpdates      bool       // continuous updates enabled by the client
	contUpdatesAcked bool       // EndOfContinuousUpdates already sent for support ack
	forceContinuous  bool       // server forces proactive push regardless of client mode
	started          bool       // client has issued at least one FramebufferUpdateRequest
	latest           *frameData // most recent frame awaiting delivery (coalescing formats)
	hasLatest        bool
	h264Q            []*frameData  // ordered H.264 access units (no drop; see offerFrame)
	h264QCond        *sync.Cond    // signalled when the H.264 queue drains (backpressure)
	closing          bool          // set under stateMu on close to release blocked producers
	wake             chan struct{} // signals writeLoop that state changed

	frameCh chan *frameData
	done    chan struct{}
	once    sync.Once
	writeMu sync.Mutex // serializes all writes to conn after handshake

	zlibStream *zlibStream // persistent stream for zlib encoding
	zrleStream *zlibStream // persistent stream for ZRLE encoding

	videoH264Want    bool // last signalled "wants H.264" state
	videoH264WantSet bool

	firstConsumer bool      // whether this client's connect should start capture
	readyOnce     sync.Once // guards the deferred OnClientConnected trigger

	pixelTol  int    // per-pixel change tolerance |dB|+|dG|+|dR|; 0=exact
	prevFrame []byte // previous frame's BGRA pixels for dirty diffing
	prevW     int    // dimensions of prevFrame
	prevH     int
	needFull  bool // force a full (non-incremental) update next frame

	pixelNeed    bool // whether this client currently needs raw pixels
	pixelNeedSet bool // whether pixelNeed has been reported at least once

	lastEnc    int32 // last chosen frame encoding (for change logging)
	lastEncSet bool
	lastFormat capture.VideoFormat

	mouseX, mouseY int // previous absolute position for computing deltas
	lastButtons    uint8
	pressedKeys    map[uint8]bool // currently pressed HID keycodes
	mouseSkip      bool           // skip first mouse event (initial position)

	audioMu        sync.Mutex // serializes negotiation, conversion and audio writes
	audioSupported bool       // client advertised QEMU Audio (-259) pseudo-encoding
	audioActive    bool       // client enabled a valid format
	audioFormat    qemuAudioFormat
	audioConverter qemuAudioConverter

	// QEMU Pointer Motion Change (-263): the VNC equivalent of a pointer lock.
	// When mouseRelative is true, the client (TigerVNC / noVNC / Remmina) grabs
	// the mouse, warps the cursor to the framebuffer center on every motion, and
	// sends PointerEvent with center-relative coordinates. The server computes
	// the real delta and forwards it as a relative mouse report to the HID layer.
	qemuMouseRel  bool // client advertised -263 in SetEncodings
	mouseRelative bool // server has requested relative mouse mode via QEMU message

	cursorCap  capture.CursorCapture
	curLogged  bool
	cursorHash [32]byte
	cursorSent bool
}

type frameData struct {
	timing videotiming.Delivery
	buffer *framebuf.Buffer
	format capture.VideoFormat
	data   []byte
	width  uint16
	height uint16
}

func (f *frameData) release() {
	if f != nil {
		f.buffer.Release()
	}
}
func (f *frameData) retain() *frameData {
	out := *f
	out.buffer = framebuf.Acquire(f.data, f.buffer)
	if f.buffer == nil {
		out.data = out.buffer.Bytes()
	}
	return &out
}

func (c *client) supportsTight() bool {
	return c.hasEncoding(encTight)
}

func (c *client) supportsJPEG() bool {
	return c.hasEncoding(encJPEG)
}

func (c *client) supportsHextile() bool {
	return c.hasEncoding(encHextile)
}

func (c *client) supportsZlib() bool {
	return c.hasEncoding(encZlib)
}

func (c *client) supportsZRLE() bool {
	return c.hasEncoding(encZRLE)
}

func (c *client) supportsOpenH264() bool {
	return c.hasEncoding(encH264)
}

func (c *client) supportsQEMUAudio() bool {
	return c.hasEncoding(encQEMUAudio)
}

func (c *client) supportsCursor() bool {
	return c.hasEncoding(encCursor)
}

func (c *client) supportsCursorWithAlpha() bool {
	return c.hasEncoding(encCursorWithAlpha)
}

// hasEncoding reports whether the client advertised the given encoding.
// Safe for concurrent use with readLoop updating the encoding list.
func (c *client) hasEncoding(enc int32) bool {
	c.encMu.Lock()
	defer c.encMu.Unlock()
	return c.hasEncodingLocked(enc)
}

// hasEncodingIn reports whether enc appears in the given encoding slice.
func hasEncodingIn(encs []int32, enc int32) bool {
	for _, e := range encs {
		if e == enc {
			return true
		}
	}
	return false
}

// pixelFormatSnapshot returns the negotiated pixel format under lock.
func (c *client) pixelFormatSnapshot() pixelFormat {
	c.encMu.Lock()
	defer c.encMu.Unlock()
	if c.pixelFmt.bpp == 0 {
		return defaultPixelFormat()
	}
	return c.pixelFmt
}

// supportsJPEGQuality reports whether the client advertised a JPEG Quality
// Level pseudo-encoding (-23..-32). Per the RFB spec, Tight JpegCompression
// may only be used when the client has advertised a quality level.
func (c *client) supportsJPEGQuality() bool {
	c.encMu.Lock()
	defer c.encMu.Unlock()
	for _, enc := range c.encodings {
		if enc >= encJPEGQualityLow && enc <= encJPEGQualityHigh {
			return true
		}
	}
	return false
}

// supportsDesktopSize reports whether the client advertised the DesktopSize
// pseudo-encoding, allowing the server to change the framebuffer dimensions.
func (c *client) supportsDesktopSize() bool {
	return c.hasEncoding(encDesktopSize)
}

// jpegQualityFromEncodings maps the JPEG Quality Level pseudo-encoding
// (-23 = highest .. -32 = lowest) advertised by the client to a JPEG quality
// value in the range 10..95. Returns 0 if no quality level was advertised.
func jpegQualityFromEncodings(encs []int32) int {
	for _, enc := range encs {
		if enc >= encJPEGQualityLow && enc <= encJPEGQualityHigh {
			// level 0 (=-32, lowest) .. 9 (=-23, highest)
			level := int(enc - encJPEGQualityLow)
			// Map 0..9 onto 10..95.
			return 10 + level*85/9
		}
	}
	return 0
}

// jpegQualityValue returns the JPEG quality (1-100) to encode at, under lock.
// A configured jpegLock overrides the client's negotiated quality; otherwise
// the client's advertised quality level is used, falling back to
// defaultJPEGQuality when the client did not specify one.
func (c *client) jpegQualityValue() int {
	c.encMu.Lock()
	defer c.encMu.Unlock()
	if c.jpegLock > 0 {
		return c.jpegLock
	}
	if c.jpegQuality <= 0 {
		return defaultJPEGQuality
	}
	return c.jpegQuality
}

func (c *client) logf(format string, args ...interface{}) {
	prefix := fmt.Sprintf("vnc: %s: ", c.conn.RemoteAddr())
	log.Printf(prefix+format, args...)
}

func (c *client) close() {
	c.once.Do(func() {
		close(c.done)
		c.conn.Close()
		// Release any producer blocked on the H.264 queue backpressure.
		c.stateMu.Lock()
		c.closing = true
		c.latest.release()
		c.latest = nil
		c.hasLatest = false
		for _, f := range c.h264Q {
			f.release()
		}
		c.h264Q = nil
		c.stateMu.Unlock()
		if c.h264QCond != nil {
			c.h264QCond.Broadcast()
		}
	})
}

// writeFull writes all provided byte slices to the connection atomically with
// respect to other post-handshake writers (frame updates, DesktopSize, etc.).
func (c *client) writeFull(parts ...[]byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	for _, p := range parts {
		if len(p) == 0 {
			continue
		}
		if _, err := c.conn.Write(p); err != nil {
			return err
		}
	}
	return nil
}

func (c *client) run() {
	defer func() {
		c.close()
		c.server.removeClient(c)
	}()

	if err := c.handshake(); err != nil {
		c.logf("handshake error: %v", err)
		return
	}

	c.logf("handshake complete, adding client")
	c.server.addClient(c)

	// Start capture once the client has announced its encodings (see markReady),
	// so the capture opens directly in the negotiated codec. This fallback
	// guarantees capture still starts if the client never sends SetEncodings.
	time.AfterFunc(500*time.Millisecond, c.markReady)

	go c.readLoop()
	c.writeLoop()
}

func (c *client) handshake() error {
	if _, err := io.WriteString(c.conn, protoVersion); err != nil {
		return fmt.Errorf("send version: %w", err)
	}

	buf := make([]byte, 12)
	if _, err := io.ReadFull(c.conn, buf); err != nil {
		return fmt.Errorf("read version: %w", err)
	}

	hasPassword := c.server.config.Password != ""
	secTypes := make([]byte, 0, 3)
	if c.server.tlsConfig != nil {
		secTypes = append(secTypes, secTypeVeNCrypt)
	}
	if hasPassword {
		secTypes = append(secTypes, secTypeVNC)
	} else {
		secTypes = append(secTypes, secTypeNone)
	}
	if _, err := c.conn.Write(append([]byte{byte(len(secTypes))}, secTypes...)); err != nil {
		return fmt.Errorf("send security: %w", err)
	}
	c.logf("offered security types %s", secTypesString(secTypes))

	var secType [1]byte
	if _, err := io.ReadFull(c.conn, secType[:]); err != nil {
		return fmt.Errorf("read security type: %w", err)
	}
	c.logf("client chose security type %s", secTypeName(secType[0]))

	switch secType[0] {
	case secTypeNone:
		if hasPassword {
			c.sendAuthFailed("password required")
			return fmt.Errorf("client chose none but password is required")
		}
		if _, err := c.conn.Write([]byte{0, 0, 0, secResultOK}); err != nil {
			return fmt.Errorf("send sec result: %w", err)
		}

	case secTypeVNC:
		if !hasPassword {
			c.sendAuthFailed("no password configured")
			return fmt.Errorf("client chose VNC auth but no password configured")
		}
		if err := c.doVNCAuth(); err != nil {
			return fmt.Errorf("vnc auth: %w", err)
		}

	case secTypeVeNCrypt:
		if c.server.tlsConfig == nil {
			c.sendAuthFailed("VeNCrypt not available")
			return fmt.Errorf("client chose VeNCrypt but no TLS config")
		}
		if err := c.doVeNCrypt(); err != nil {
			return fmt.Errorf("vencrypt: %w", err)
		}

	default:
		c.sendAuthFailed("unsupported security type")
		return fmt.Errorf("client chose security type %s", secTypeName(secType[0]))
	}

	var shared [1]byte
	if _, err := io.ReadFull(c.conn, shared[:]); err != nil {
		return fmt.Errorf("read client init: %w", err)
	}
	c.logf("client shared flag: %d", shared[0])

	if err := c.sendServerInit(); err != nil {
		return fmt.Errorf("send server init: %w", err)
	}
	c.logf("sent ServerInit %dx%d name=%s", c.width, c.height, c.server.config.Name)

	return nil
}

func (c *client) doVNCAuth() error {
	challenge, err := generateChallenge()
	if err != nil {
		return fmt.Errorf("generate challenge: %w", err)
	}

	if _, err := c.conn.Write(challenge); err != nil {
		return fmt.Errorf("send challenge: %w", err)
	}

	var response [16]byte
	if _, err := io.ReadFull(c.conn, response[:]); err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	expected, err := vncEncrypt(challenge, c.server.config.Password)
	if err != nil {
		return fmt.Errorf("compute expected: %w", err)
	}

	if subtleEqual(expected, response[:]) {
		if _, err := c.conn.Write([]byte{0, 0, 0, secResultOK}); err != nil {
			return fmt.Errorf("send auth ok: %w", err)
		}
		c.logf("VNC auth succeeded")
		return nil
	}

	c.sendAuthFailed("invalid password")
	return fmt.Errorf("invalid password")
}

func (c *client) sendAuthFailed(reason string) {
	c.conn.Write([]byte{0, 0, 0, secResultFailed})
	reasonLen := len(reason)
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(reasonLen))
	c.conn.Write(lenBuf[:])
	c.conn.Write([]byte(reason))
}

func subtleEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func (c *client) sendServerInit() error {
	c.width = uint16(c.server.config.Width)
	c.height = uint16(c.server.config.Height)

	var buf [24]byte
	binary.BigEndian.PutUint16(buf[0:], c.width)
	binary.BigEndian.PutUint16(buf[2:], c.height)
	copy(buf[4:20], pixelFormatBGR)

	name := c.server.config.Name
	nameLen := len(name)
	binary.BigEndian.PutUint32(buf[20:], uint32(nameLen))

	parts := [][]byte{buf[:], []byte(name)}
	for _, p := range parts {
		if _, err := c.conn.Write(p); err != nil {
			return err
		}
	}
	return nil
}

func (c *client) readLoop() {
	defer func() {
		c.close()
	}()

	buf := make([]byte, 256)
	for {
		select {
		case <-c.done:
			return
		default:
		}

		if _, err := io.ReadFull(c.conn, buf[:1]); err != nil {
			c.logf("readLoop exit: %v", err)
			return
		}

		switch buf[0] {
		case msgSetPixelFormat:
			if _, err := io.ReadFull(c.conn, buf[:19]); err != nil {
				c.logf("read pixel format: %v", err)
				return
			}
			// buf[0:3] is padding; buf[3:19] is the 16-byte pixel format.
			pf := parsePixelFormat(buf[3:19])
			c.encMu.Lock()
			c.pixelFmt = pf
			c.needFull = true
			c.encMu.Unlock()
			// Note: the persistent zlib/ZRLE streams must NOT be reset here.
			// A pixel-format change is byte-transparent to zlib; injecting a
			// new zlib header mid-stream desyncs the client's inflater. The
			// stream carries whatever CPIXEL/pixel bytes we emit next.
			c.logf("SetPixelFormat: bpp=%d depth=%d bigEndian=%v rgbMax=%d/%d/%d shift=%d/%d/%d",
				pf.bpp*8, pf.depth, pf.bigEndian, pf.redMax, pf.greenMax, pf.blueMax,
				pf.redShift, pf.greenShift, pf.blueShift)
			c.refreshPixelNeed()

		case msgSetEncodings:
			if _, err := io.ReadFull(c.conn, buf[:3]); err != nil {
				c.logf("read encodings header: %v", err)
				return
			}
			n := int(binary.BigEndian.Uint16(buf[1:3]))
			encs := make([]int32, 0, n)
			for i := 0; i < n; i++ {
				if _, err := io.ReadFull(c.conn, buf[:4]); err != nil {
					c.logf("read encoding %d: %v", i, err)
					return
				}
				encs = append(encs, int32(binary.BigEndian.Uint32(buf[:4])))
			}
			c.encMu.Lock()
			c.encodings = encs
			c.jpegQuality = jpegQualityFromEncodings(encs)
			c.needFull = true
			c.encMu.Unlock()
			c.logEncodingList(encs)
			c.refreshPixelNeed()
			c.refreshVideoH264Want()

			// Track QEMU Audio (-259) capability so the server knows it can
			// push audio data to this client.
			c.setQEMUAudioCapability(hasEncodingIn(encs, encQEMUAudio))

			// Track QEMU Pointer Motion Change (-263): the server can request
			// relative mouse mode (cursor lock-to-center) for iOS BLE HID.
			c.qemuMouseRel = hasEncodingIn(encs, encQEMUPointerMotion)
			if c.qemuMouseRel && c.server.config.MouseMode != "absolute" {
				c.sendQEMUMouseMode(true)
				c.mouseRelative = true
				c.mouseX, c.mouseY = int(c.width/2), int(c.height/2)
			}

			// Encodings (and codec preference) are now known: allow capture to
			// start, opening directly in the negotiated codec.
			c.markReady()

			// Per spec, if the client advertises the ContinuousUpdates
			// pseudo-encoding, the server must send EndOfContinuousUpdates once
			// to confirm the extension is supported.
			if hasEncodingIn(encs, encContinuousUpdates) && !c.contUpdatesAcked {
				c.contUpdatesAcked = true
				c.sendEndOfContinuousUpdates()
			}

		case msgFramebufferUpdateRequest:
			if _, err := io.ReadFull(c.conn, buf[:9]); err != nil {
				c.logf("read update request: %v", err)
				return
			}
			incremental := buf[0] != 0
			c.incremental = incremental
			c.reqX = binary.BigEndian.Uint16(buf[1:3])
			c.reqY = binary.BigEndian.Uint16(buf[3:5])
			c.reqW = binary.BigEndian.Uint16(buf[5:7])
			c.reqH = binary.BigEndian.Uint16(buf[7:9])

			if !incremental {
				// Non-incremental requests force a full refresh and must be
				// honoured even while continuous updates are active.
				c.encMu.Lock()
				c.needFull = true
				c.encMu.Unlock()
			}
			// Fallback trigger: a client that requests frames without first
			// sending SetEncodings still needs capture to start.
			c.markReady()
			c.onUpdateRequest(incremental)

		case msgEnableContinuousUpdates:
			if _, err := io.ReadFull(c.conn, buf[:9]); err != nil {
				c.logf("read enable-continuous-updates: %v", err)
				return
			}
			enable := buf[0] != 0
			c.onEnableContinuousUpdates(enable)

		case msgKeyEvent:
			io.ReadFull(c.conn, buf[:7])
			c.handleKeyEvent(buf[0] != 0, binary.BigEndian.Uint32(buf[3:7]))

		case msgPointerEvent:
			io.ReadFull(c.conn, buf[:5])
			c.handlePointerEvent(buf[0], binary.BigEndian.Uint16(buf[1:3]), binary.BigEndian.Uint16(buf[3:5]))

		case msgQEMU:
			if _, err := io.ReadFull(c.conn, buf[:1]); err != nil {
				return
			}
			sub := buf[0]
			switch sub {
			case submsgQEMUAudio:
				c.handleQEMUAudio()
			default:
				c.logf("QEMU: unknown submessage %d", sub)
			}

		default:
		}
	}
}

// signalWake notifies the writeLoop that delivery state changed (new frame,
// new request, or continuous-updates toggle). Non-blocking.
func (c *client) signalWake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// h264QMax bounds the pending H.264 access-unit queue. When full, offerFrame
// blocks, throttling the capture encoder to the client's consumption rate
// instead of dropping frames (dropping an inter-frame would corrupt the stream).
// Kept at 2 (one being sent + one ready) so it pipelines without a consumer
// stall while adding at most ~2 frames of buffering latency.
const h264QMax = 2

// offerFrame hands a captured frame to the writeLoop.
//
// H.264 access units are queued in order and never dropped: dropping a P-frame
// would make every later frame undecodable until the next keyframe. The queue is
// bounded, so a slow client applies backpressure (this call blocks) which
// throttles the encoder rather than corrupting the stream.
//
// All other (self-contained) formats coalesce: the newest frame replaces any
// un-sent one, which bounds latency and drops safely.
func (c *client) offerFrame(fd *frameData) {
	c.offerFrameWithStop(fd, nil)
}

func captureStopped(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

func (c *client) offerFrameWithStop(fd *frameData, stop <-chan struct{}) {
	fd = fd.retain()
	if fd.format == capture.FormatH264 {
		c.stateMu.Lock()
		// Switching to H.264: drop any stale coalesced (JPEG/raw) frame so it
		// can't flash between access units.
		c.hasLatest = false
		c.latest.release()
		c.latest = nil
		// Only a full queue needs a cancellation watcher. Taking stateMu in
		// the watcher prevents a wakeup from being lost just before Wait.
		if len(c.h264Q) >= h264QMax && stop != nil {
			watchDone := make(chan struct{})
			defer close(watchDone)
			go func() {
				select {
				case <-stop:
					c.stateMu.Lock()
					c.h264QCond.Broadcast()
					c.stateMu.Unlock()
				case <-watchDone:
				}
			}()
		}
		for len(c.h264Q) >= h264QMax && !c.closing && !captureStopped(stop) {
			c.h264QCond.Wait()
		}
		if c.closing || captureStopped(stop) {
			fd.release()
			c.stateMu.Unlock()
			return
		}
		c.h264Q = append(c.h264Q, fd)
		c.stateMu.Unlock()
		c.signalWake()
		return
	}

	c.stateMu.Lock()
	if c.closing || captureStopped(stop) {
		c.stateMu.Unlock()
		fd.release()
		return
	}
	// Switching away from H.264: discard obsolete queued access units and
	// release any producer blocked on backpressure.
	if len(c.h264Q) > 0 {
		for _, f := range c.h264Q {
			f.release()
		}
		c.h264Q = nil
		c.h264QCond.Broadcast()
	}
	c.latest.release()
	c.latest = fd
	c.hasLatest = true
	c.stateMu.Unlock()
	c.signalWake()
}

// onUpdateRequest arms a pending framebuffer update. Per the RFB spec, while
// continuous updates are active the server must ignore incremental requests but
// still honour non-incremental (full) requests.
func (c *client) onUpdateRequest(incremental bool) {
	c.stateMu.Lock()
	c.started = true
	if !c.contUpdates || !incremental {
		c.pendingReq = true
	}
	c.stateMu.Unlock()
	c.signalWake()
}

// onEnableContinuousUpdates handles the EnableContinuousUpdates client message.
// Enabling switches to proactive pushing; disabling reverts to request-driven
// delivery and requires sending an EndOfContinuousUpdates message.
func (c *client) onEnableContinuousUpdates(enable bool) {
	c.stateMu.Lock()
	c.contUpdates = enable
	c.stateMu.Unlock()

	if enable {
		c.logf("continuous updates enabled")
	} else {
		// Spec: must immediately send EndOfContinuousUpdates, even if it was
		// already disabled.
		c.sendEndOfContinuousUpdates()
		c.logf("continuous updates disabled")
	}
	c.signalWake()
}

// sendEndOfContinuousUpdates sends the EndOfContinuousUpdates server message.
func (c *client) sendEndOfContinuousUpdates() {
	if err := c.writeFull([]byte{msgEndOfContinuousUpdates}); err != nil {
		c.logf("sendEndOfContinuousUpdates write: %v", err)
		c.close()
	}
}

func (c *client) writeLoop() {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-c.wake:
		case <-ticker.C:
		}

		for {
			c.stateMu.Lock()
			// Deliver when allowed to push: a pending request (request-driven),
			// the client enabled continuous updates, or the server forces
			// continuous push (ForceContinuous, which only kicks in after the
			// client's first request so encodings/pixel format are negotiated).
			allowed := c.pendingReq || c.contUpdates || (c.forceContinuous && c.started)
			var frame *frameData
			switch {
			case allowed && len(c.h264Q) > 0:
				frame = c.h264Q[0]
				c.h264Q = c.h264Q[1:]
				c.pendingReq = false
				c.h264QCond.Signal()
			case allowed && c.hasLatest:
				frame = c.latest
				c.latest = nil
				c.hasLatest = false
				c.pendingReq = false
			}
			c.stateMu.Unlock()

			if frame == nil {
				break
			}
			c.sendFrame(frame)
			frame.release()
		}
		// Cursor updates are independent of desktop pixels. Reserve a pending
		// request before writing; a new request arriving during the write survives.
		c.stateMu.Lock()
		pending := c.pendingReq
		allowed := pending || c.contUpdates || (c.forceContinuous && c.started)
		if allowed {
			c.pendingReq = false
		}
		c.stateMu.Unlock()
		if allowed && !c.sendCursorIfChanged() && pending {
			c.stateMu.Lock()
			c.pendingReq = true
			c.stateMu.Unlock()
		}
	}
}

func (c *client) sendFrame(frame *frameData) {
	if frame == nil {
		c.logf("sendFrame: nil frame")
		return
	}
	// Degenerate frames (unknown size) cannot be encoded; dropping them also
	// protects the tile encoders from division by zero.
	if frame.width == 0 || frame.height == 0 {
		c.logf("sendFrame: dropping frame with zero dimensions")
		return
	}

	pf := c.pixelFormatSnapshot()

	// If the framebuffer size changed since the client's last known size,
	// announce it via the DesktopSize pseudo-encoding on this same goroutine,
	// so it is strictly ordered before the pixel data that follows.
	if frame.width != c.width || frame.height != c.height {
		if c.supportsDesktopSize() {
			if !c.sendDesktopSize(frame.width, frame.height) {
				return
			}
		}
		c.width = frame.width
		c.height = frame.height
	}

	// Determine which encoder to use for this frame's rectangles.
	enc := c.chooseEncoding(pf, frame)
	if frame.format == capture.FormatH264 && enc != encH264 {
		// A stale capture frame may race with a negotiation change.
		c.stateMu.Lock()
		c.pendingReq = true
		c.stateMu.Unlock()
		return
	}
	// A server quality lock must also apply to compressed capture input.
	// Decode here when the source cannot provide raw pixels (or a stale MJPEG
	// frame races with the raw-pixel request), then encode dirty rectangles at
	// the locked quality instead of silently forwarding source-quality JPEG.
	if frame.format == capture.FormatJPEG && (c.jpegLock > 0 || (enc != encJPEG && enc != encTight)) {
		pixels, w, h, err := jpegcodec.DecodeBGRA(frame.data)
		if err != nil || w != int(frame.width) || h != int(frame.height) {
			c.close()
			return
		}
		decoded := *frame
		decoded.data, decoded.format = pixels, capture.FormatRaw
		frame = &decoded
	}

	// Open H.264 forwards the capture-produced access unit directly as a single
	// full-framebuffer rectangle, bypassing the dirty-rect machinery.
	if enc == encH264 {
		c.logEncoding(enc, pf, frame.format)
		// Each frame.data from readH264Frame is already a complete access unit
		// (extracted by the push-based Annex-B parser in capture_ffmpeg.go).
		c.sendH264Update(int(frame.width), int(frame.height), frame.data, 0)
		return
	}

	// Raw pixels share 64px dirty detection, including JPEG rectangle encoding.
	// Already compressed MJPEG remains whole-frame passthrough.
	effTile := c.effectiveTileSize(enc)
	if frame.format == capture.FormatJPEG {
		effTile = 0
	}

	// A full refresh is forced on the first frame, size change, pixel-format
	// change, or when the client requested a non-incremental update.
	c.encMu.Lock()
	full := c.needFull
	c.needFull = false
	c.encMu.Unlock()

	var rects []rectangle
	fullRect := []rectangle{{0, 0, int(frame.width), int(frame.height)}}
	if effTile <= 0 {
		// Dirty-region detection disabled for this encoding: send full frame.
		rects = fullRect
	} else if len(frame.data) == 0 {
		// No raw pixels (e.g. JPEG-only source); send one full rectangle.
		rects = fullRect
	} else if full || c.prevFrame == nil ||
		c.prevW != int(frame.width) || c.prevH != int(frame.height) {
		rects = fullRect
	} else {
		rects = computeDirtyRects(c.prevFrame, frame.data,
			int(frame.width), int(frame.height), c.pixelTol)
		if len(rects) == 0 {
			// Nothing changed within tolerance: keep the existing baseline
			// (do NOT overwrite it, so sub-tolerance drift accumulates against
			// the last transmitted pixels and self-corrects once it exceeds
			// the tolerance) and skip sending. Preserve the outstanding request
			// so a later changed frame can satisfy a request-driven viewer.
			c.stateMu.Lock()
			c.pendingReq = true
			c.stateMu.Unlock()
			return
		}
	}

	if effTile > 0 && (enc == encJPEG || enc == encTight) {
		rects = splitJPEGRects(rects)
	}
	c.logEncoding(enc, pf, frame.format)

	var curEnc int32
	var curData []byte
	var curW, curH, curHotX, curHotY uint16
	if cs, ok := c.captureCursorState(); ok {
		curEnc, curData, curW, curH, curHotX, curHotY = c.prepareCursor(cs)
		if curEnc != 0 && !c.curLogged {
			c.logf("cursor: %dx%d hot=(%d,%d) size=%d enc=%s", curW, curH, curHotX, curHotY, len(curData), encodingName(curEnc))
			c.curLogged = true
		}
	}

	if !c.sendFrameUpdate(enc, curEnc, curData, curW, curH, curHotX, curHotY, frame, pf, rects) {
		return
	}
	if effTile > 0 {
		// Update the baseline only for the rectangles we actually transmitted.
		// Unchanged tiles retain their previous baseline so that tolerance
		// drift is measured cumulatively against the last sent pixels.
		c.updatePrevRects(frame, rects)
	} else {
		// Detection disabled for this encoding: invalidate the cached frame so
		// that if a tiled encoding is used again later, it forces a full
		// refresh rather than diffing against a stale frame.
		c.prevFrame = nil
	}
}

// effectiveTileSize returns the tile size used for dirty-region detection for
// the given encoding on raw pixels. Compressed source frames bypass detection.
func (c *client) effectiveTileSize(enc int32) int {
	switch enc {
	case encZRLE, encHextile, encZlib, encRaw, encJPEG, encTight:
		return pixelops.BlockSize
	default:
		return 0
	}
}

// updatePrevRects copies the pixels of the given (transmitted) rectangles from
// the current frame into the baseline prevFrame, allocating/resizing it as
// needed. Only these regions are updated; other regions keep their previous
// baseline pixels.
func (c *client) updatePrevRects(frame *frameData, rects []rectangle) {
	n := len(frame.data)
	if n == 0 {
		return
	}
	w := int(frame.width)
	if c.prevFrame == nil || c.prevW != w || c.prevH != int(frame.height) || len(c.prevFrame) != n {
		// Baseline missing or size changed: reallocate and copy the full frame.
		if cap(c.prevFrame) >= n {
			c.prevFrame = c.prevFrame[:n]
		} else {
			c.prevFrame = make([]byte, n)
		}
		copy(c.prevFrame, frame.data)
		c.prevW = w
		c.prevH = int(frame.height)
		return
	}
	stride := w * 4
	for _, r := range rects {
		rowBytes := r.w * 4
		for y := 0; y < r.h; y++ {
			off := (r.y+y)*stride + r.x*4
			copy(c.prevFrame[off:off+rowBytes], frame.data[off:off+rowBytes])
		}
	}
}

// chooseEncoding selects the RFB encoding for a frame based on the format the
// capture layer produced (H.264 / JPEG / raw pixels) and the client's advertised
// capabilities. The capture format is driven by prior negotiation (H.264 want,
// pixel need), so it always matches what the client can consume.
func (c *client) chooseEncoding(pf pixelFormat, frame *frameData) int32 {
	if frame.format == capture.FormatH264 {
		if c.supportsOpenH264() && len(frame.data) > 0 {
			return encH264
		}
		return encRaw
	}
	// JPEG-capable viewers accept either whole-frame MJPEG passthrough or
	// independently encoded dirty rectangles from raw capture pixels.
	if c.supportsTight() && c.supportsJPEGQuality() && pf.bpp >= 2 {
		return encTight
	}
	if c.supportsJPEG() && pf.bpp >= 2 {
		return encJPEG
	}
	if c.supportsZRLE() {
		return encZRLE
	}
	if c.supportsZlib() {
		return encZlib
	}
	if c.supportsHextile() {
		return encHextile
	}
	return encRaw
}

// refreshPixelNeed recomputes whether this client requires raw pixels and, if
// the value changed, reports it upward via the server callback.
//
// JPEG/Tight viewers can accept device MJPEG without raw pixels when quality
// is unlocked. A server quality lock requests pixels for dirty JPEG encoding;
// sendFrame also decodes any compressed frames the source still supplies.
func (c *client) refreshPixelNeed() {
	c.encMu.Lock()
	pf := c.pixelFmt
	if pf.bpp == 0 {
		pf = defaultPixelFormat()
	}
	jpegOnly := ((c.hasEncodingLocked(encTight) && c.jpegQualityLocked()) ||
		c.hasEncodingLocked(encJPEG)) && pf.bpp >= 2
	qualityLocked := c.jpegLock > 0
	c.encMu.Unlock()

	need := !jpegOnly || qualityLocked

	c.stateMu.Lock()
	changed := !c.pixelNeedSet || c.pixelNeed != need
	c.pixelNeed = need
	c.pixelNeedSet = true
	c.stateMu.Unlock()

	if changed {
		c.server.notifyPixelNeed(need)
		c.logf("pixel need: %v (jpegOnly=%v)", need, jpegOnly)
	}
}

// refreshVideoH264Want recomputes whether this client wants H.264 video (it
// advertised the Open H.264 encoding) and, if changed, reports it upward so the
// capture layer can switch its output codec.
func (c *client) refreshVideoH264Want() {
	want := c.supportsOpenH264()

	changed := !c.videoH264WantSet || c.videoH264Want != want
	c.videoH264Want = want
	c.videoH264WantSet = true

	if changed {
		c.server.notifyVideoH264(want)
		c.logf("video H.264 want: %v", want)
	}
}

// markReady triggers the deferred capture start exactly once, after the client's
// encodings (and thus its desired codec) are known. Runs the callback on a
// separate goroutine so opening the capture device never blocks the read loop.
func (c *client) markReady() {
	c.readyOnce.Do(func() {
		select {
		case <-c.done:
			return
		default:
		}
		go c.server.clientReady(c)
	})
}

// hasEncodingLocked reports whether enc is advertised. Caller must hold encMu.
func (c *client) hasEncodingLocked(enc int32) bool {
	if !c.codecs.allows(enc) {
		return false
	}
	for _, e := range c.encodings {
		if e == enc {
			return true
		}
	}
	return false
}

// jpegQualityLocked reports whether a JPEG quality level was advertised.
// Caller must hold encMu.
func (c *client) jpegQualityLocked() bool {
	for _, enc := range c.encodings {
		if enc >= encJPEGQualityLow && enc <= encJPEGQualityHigh {
			return true
		}
	}
	return false
}

// sendRects encodes the given rectangles with the chosen encoding and writes a
// single FramebufferUpdate message. Returns false on encoding or write error.
func (c *client) sendRects(enc int32, frame *frameData, pf pixelFormat, rects []rectangle) bool {
	var buf bytes.Buffer

	var hdr [4]byte
	hdr[0] = msgFramebufferUpdate
	hdr[1] = 0
	binary.BigEndian.PutUint16(hdr[2:], uint16(len(rects)))
	buf.Write(hdr[:])

	for _, r := range rects {
		if err := c.encodeRect(&buf, enc, frame, pf, r); err != nil {
			c.logf("encode rectangle: %v", err)
			c.close()
			return false
		}
	}

	var started time.Time
	if frame.timing.Trace != nil {
		started = time.Now()
	}
	if err := c.writeFull(buf.Bytes()); err != nil {
		c.logf("sendRects write: %v", err)
		c.close()
		return false
	}
	frame.timing.Sent("vnc", started)
	return true
}

// encodeRect appends one rectangle (header + encoded body) to buf.
func (c *client) encodeRect(buf *bytes.Buffer, enc int32, frame *frameData, pf pixelFormat, r rectangle) error {
	var rhdr [12]byte
	binary.BigEndian.PutUint16(rhdr[0:], uint16(r.x))
	binary.BigEndian.PutUint16(rhdr[2:], uint16(r.y))
	binary.BigEndian.PutUint16(rhdr[4:], uint16(r.w))
	binary.BigEndian.PutUint16(rhdr[6:], uint16(r.h))
	binary.BigEndian.PutUint32(rhdr[8:], uint32(enc))
	buf.Write(rhdr[:])

	switch enc {
	case encTight:
		return c.encodeTightBody(buf, frame, r)
	case encJPEG:
		return c.encodeJPEGBody(buf, frame, r)
	case encZRLE:
		c.encodeZRLEBody(buf, frame, pf, r)
	case encZlib:
		c.encodeZlibBody(buf, frame, pf, r)
	case encHextile:
		c.encodeHextileBody(buf, frame, pf, r)
	default:
		c.encodeRawBody(buf, frame, pf, r)
	}
	return nil
}

// logEncoding logs the chosen frame encoding whenever it changes.
func (c *client) logEncoding(enc int32, pf pixelFormat, format capture.VideoFormat) {
	if c.lastEncSet && c.lastEnc == enc && c.lastFormat == format {
		return
	}
	c.lastEnc = enc
	c.lastEncSet = true
	c.lastFormat = format
	encoder := "Go"
	switch enc {
	case encH264:
		encoder = "passthrough"
	case encJPEG, encTight:
		encoder = jpegcodec.Backend()
		if format == capture.FormatJPEG {
			encoder = "passthrough"
		}
	case encZlib, encZRLE:
		encoder = "Go compress/zlib"
	case encRaw:
		encoder = "none (pixels)"
	}
	if enc == encTight || enc == encJPEG {
		c.logf("video codec=%s encoder=%q (bpp=%d, jpegQuality=%d)",
			encodingName(enc), encoder, pf.bpp*8, c.jpegQualityValue())
	} else {
		c.logf("video codec=%s encoder=%q (bpp=%d)", encodingName(enc), encoder, pf.bpp*8)
	}
}

// encodingName returns a human-readable name for an encoding or pseudo-encoding.
func encodingName(enc int32) string {
	switch enc {
	case encRaw:
		return "Raw"
	case encCopyRect:
		return "CopyRect"
	case 2:
		return "RRE"
	case 4:
		return "CoRRE"
	case encHextile:
		return "Hextile"
	case encZlib:
		return "Zlib"
	case encTight:
		return "Tight"
	case 8:
		return "zlibhex"
	case 9:
		return "Ultra(UltraVNC)"
	case 10:
		return "Ultra2(UltraVNC)"
	case 15:
		return "TRLE"
	case encZRLE:
		return "ZRLE"
	case 17:
		return "ZYWRLE"
	case 18:
		return "XZ(UltraVNC)"
	case 19:
		return "XZYW(UltraVNC)"
	case 25:
		return "Zstd(UltraVNC)"
	case 26:
		return "TightZstd(UltraVNC)"
	case 27:
		return "ZstdHex(UltraVNC)"
	case 28:
		return "ZSTDRLE(UltraVNC)"
	case 29:
		return "ZSTDYWRLE(UltraVNC)"
	case 20:
		return "H.264"
	case 21:
		return "JPEG"
	case 22:
		return "JRLE"
	case 23:
		return "VAH.264"
	case 24:
		return "ZRLE2"
	case 50:
		return "OpenH.264"
	case -260:
		return "TightPNG"
	case encCursor:
		return "Cursor(pseudo)"
	case encXCursor:
		return "XCursor(pseudo)"
	case encCursorWithAlpha:
		return "CursorWithAlpha(pseudo)"
	case encQEMUPointerMotion:
		return "QEMUPtrMotion(pseudo)"
	case encDesktopSize:
		return "DesktopSize(pseudo)"
	case -224:
		return "LastRect(pseudo)"
	case -308:
		return "ExtendedDesktopSize(pseudo)"
	case -307:
		return "DesktopName(pseudo)"
	case -312:
		return "Fence(pseudo)"
	case -313:
		return "ContinuousUpdates(pseudo)"
	case -258:
		return "QEMUExtendedKeyEvent(pseudo)"
	case encQEMUAudio:
		return "QEMUAudio(pseudo)"
	case -261:
		return "QEMULEDState(pseudo)"
	case -309:
		return "Xvp(pseudo)"
	case -232:
		return "PointerPos(pseudo)"
	case -131072:
		return "Clipboard(pseudo)"
	case -131071:
		return "ClipboardEnable(pseudo)"
	case -131070:
		return "ClipboardPeek(pseudo)"
	case -131069:
		return "ClipboardNotify(pseudo)"
	case -316:
		return "ExtendedMouseButtons(pseudo)"
	case int32(0x574D5664):
		return "VMwareCursor(pseudo)"
	case int32(0x574D5665):
		return "VMwareCursorState(pseudo)"
	case int32(0x574D5666):
		return "VMwareCursorPosition(pseudo)"
	case int32(0x574D5668):
		return "VMwareLEDState(pseudo)"
	case int32(0xC0A1E5CE - 1<<32): // 0xC0A1E5CE
		return "ExtendedClipboard(pseudo)"
	}
	if enc >= encJPEGQualityLow && enc <= encJPEGQualityHigh {
		return fmt.Sprintf("JPEGQuality[%d](pseudo)", enc-encJPEGQualityLow)
	}
	if enc >= -256 && enc <= -247 {
		return fmt.Sprintf("CompressLevel[%d](pseudo)", enc+256)
	}
	if enc >= int32(0x574D5600) && enc <= int32(0x574D56FF) {
		return fmt.Sprintf("VMware[0x%08X](pseudo)", uint32(enc))
	}
	if (enc >= 1000 && enc <= 1002) || enc == 1011 || (enc >= 1100 && enc <= 1105) {
		return fmt.Sprintf("Apple[%d]", enc)
	}
	if name := ultraVNCPseudoName(enc); name != "" {
		return name
	}
	return fmt.Sprintf("enc(%d)", enc)
}

// ultraVNCPseudoName names UltraVNC-specific pseudo-encodings: the Cache/Zip
// family (0xFFFF0000..) and the feature-negotiation family (0xFFFF8000..).
// Returns "" if enc is not one of them.
func ultraVNCPseudoName(enc int32) string {
	switch uint32(enc) {
	case 0xFFFF0000:
		return "UltraVNC:Cache(pseudo)"
	case 0xFFFF0001:
		return "UltraVNC:CacheEnable(pseudo)"
	case 0xFFFF0007:
		return "UltraVNC:CacheZip(pseudo)"
	case 0xFFFF0008:
		return "UltraVNC:QueueZip(pseudo)"
	case 0xFFFF0009:
		return "UltraVNC:UltraZip(pseudo)"
	case 0xFFFF000A:
		return "UltraVNC:QueueZstd(pseudo)"
	case 0xFFFF000B:
		return "UltraVNC:QueueEnable(pseudo)"
	case 0xFFFF8000:
		return "UltraVNC:ServerState(pseudo)"
	case 0xFFFF8001:
		return "UltraVNC:EnableKeepAlive(pseudo)"
	case 0xFFFF8002:
		return "UltraVNC:FTProtocolVersion(pseudo)"
	case 0xFFFF8003:
		return "UltraVNC:Session(pseudo)"
	case 0xFFFF8004:
		return "UltraVNC:EnableIdleTime(pseudo)"
	case 0xFFFF8005:
		return "UltraVNC:MonitorInfo(pseudo)"
	case 0xFFFF8006:
		return "UltraVNC:UnicodeTextChat(pseudo)"
	case 0xFFFF8007:
		return "UltraVNC:ChatFileTransfer(pseudo)"
	}
	return ""
}

// logEncodingList logs the full list of encodings advertised by the client.
func (c *client) logEncodingList(encs []int32) {
	names := make([]string, len(encs))
	for i, e := range encs {
		names[i] = encodingName(e)
	}
	c.logf("client supports %d encodings: %s", len(encs), strings.Join(names, ", "))
}

// sendDesktopSize sends a DesktopSize pseudo-encoding rectangle to inform the
// client of new framebuffer dimensions. Returns false on write error.
func (c *client) sendDesktopSize(width, height uint16) bool {
	var hdr [16]byte
	off := 0
	off = writeU8(hdr[:], off, msgFramebufferUpdate)
	off = writeU8(hdr[:], off, 0)
	off = writeU16(hdr[:], off, 1)
	off = writeU16(hdr[:], off, 0)
	off = writeU16(hdr[:], off, 0)
	off = writeU16(hdr[:], off, width)
	off = writeU16(hdr[:], off, height)
	off = writeI32(hdr[:], off, encDesktopSize)

	if err := c.writeFull(hdr[:]); err != nil {
		c.logf("sendDesktopSize write: %v", err)
		c.close()
		return false
	}
	return true
}

func (c *client) sendFrameUpdate(pixelEnc, curEnc int32, curData []byte, curW, curH, curHotX, curHotY uint16, frame *frameData, pf pixelFormat, rects []rectangle) bool {
	nr := uint16(len(rects))
	hasCursor := curEnc != 0
	if hasCursor {
		nr++
	}

	var buf bytes.Buffer
	buf.WriteByte(msgFramebufferUpdate)
	buf.WriteByte(0)
	var nrBuf [2]byte
	binary.BigEndian.PutUint16(nrBuf[:], nr)
	buf.Write(nrBuf[:])

	if hasCursor {
		c.writeCursorRectHeader(&buf, curEnc, curW, curH, curHotX, curHotY)
		buf.Write(curData)
	}
	for _, r := range rects {
		if err := c.encodeRect(&buf, pixelEnc, frame, pf, r); err != nil {
			c.logf("encode rectangle: %v", err)
			c.close()
			return false
		}
	}

	var started time.Time
	if frame.timing.Trace != nil {
		started = time.Now()
	}
	if err := c.writeFull(buf.Bytes()); err != nil {
		c.logf("sendFrameUpdate write: %v", err)
		c.close()
		return false
	}
	frame.timing.Sent("vnc", started)
	return true
}

// encodeRawBody appends Raw-encoded pixels for rect r (converted to the
// negotiated pixel format).
func (c *client) encodeRawBody(buf *bytes.Buffer, frame *frameData, pf pixelFormat, r rectangle) {
	sub := cropBGRA(frame.data, int(frame.width), r)
	buf.Write(pf.convertFrame(sub))
}

// jpegForRect preserves device MJPEG bytes, or encodes just the requested
// rectangle from raw BGRA at the client's negotiated (or locked) quality.
func (c *client) jpegForRect(frame *frameData, r rectangle) ([]byte, error) {
	if frame.format == capture.FormatJPEG {
		return frame.data, nil
	}
	return encodeJPEG(cropBGRA(frame.data, int(frame.width), r), r.w, r.h, c.jpegQualityValue())
}

// encodeTightBody appends a Tight(JPEG) rectangle body for rect r.
func (c *client) encodeTightBody(buf *bytes.Buffer, frame *frameData, r rectangle) error {
	jpegData, err := c.jpegForRect(frame, r)
	if err != nil {
		return err
	}
	if len(jpegData) > 0x3fffff {
		return fmt.Errorf("Tight JPEG exceeds compact length limit: %d", len(jpegData))
	}
	buf.WriteByte(tightJPEG)
	var clen [3]byte
	n := encodeCompactLen(clen[:], len(jpegData))
	buf.Write(clen[:n])
	buf.Write(jpegData)
	return nil
}

// encodeJPEGBody appends a standalone JPEG (21) rectangle body for rect r.
func (c *client) encodeJPEGBody(buf *bytes.Buffer, frame *frameData, r rectangle) error {
	jpegData, err := c.jpegForRect(frame, r)
	if err != nil {
		return err
	}
	buf.Write(jpegData)
	return nil
}

// encodeHextileBody appends a Hextile rectangle body for rect r.
func (c *client) encodeHextileBody(buf *bytes.Buffer, frame *frameData, pf pixelFormat, r rectangle) {
	sub := pf.convertFrame(cropBGRA(frame.data, int(frame.width), r))
	buf.Write(encodeHextile(sub, r.w, r.h, pf.bpp))
}

// encodeZlibBody appends a zlib rectangle body for rect r using the persistent
// per-connection zlib stream.
func (c *client) encodeZlibBody(buf *bytes.Buffer, frame *frameData, pf pixelFormat, r rectangle) {
	sub := pf.convertFrame(cropBGRA(frame.data, int(frame.width), r))
	if c.zlibStream == nil {
		c.zlibStream = newZlibStream()
	}
	compressed := c.zlibStream.compress(sub)
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(compressed)))
	buf.Write(lenBuf[:])
	buf.Write(compressed)
}

// encodeZRLEBody appends a ZRLE rectangle body for rect r using the persistent
// per-connection zlib stream.
func (c *client) encodeZRLEBody(buf *bytes.Buffer, frame *frameData, pf pixelFormat, r rectangle) {
	sub := cropBGRA(frame.data, int(frame.width), r)
	tiles := buildZRLETiles(sub, r.w, r.h, pf)
	if c.zrleStream == nil {
		c.zrleStream = newZlibStream()
	}
	compressed := c.zrleStream.compress(tiles)
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(compressed)))
	buf.Write(lenBuf[:])
	buf.Write(compressed)
}

// sendH264Update writes a single FramebufferUpdate carrying one full-frame
// Open H.264 rectangle: [length][flags][data]. Returns false on write error.
func (c *client) sendH264Update(w, h int, data []byte, flags uint32) bool {
	var buf bytes.Buffer

	var hdr [4]byte
	hdr[0] = msgFramebufferUpdate
	hdr[1] = 0
	binary.BigEndian.PutUint16(hdr[2:], 1)
	buf.Write(hdr[:])

	var rhdr [12]byte
	binary.BigEndian.PutUint16(rhdr[0:], 0)
	binary.BigEndian.PutUint16(rhdr[2:], 0)
	binary.BigEndian.PutUint16(rhdr[4:], uint16(w))
	binary.BigEndian.PutUint16(rhdr[6:], uint16(h))
	binary.BigEndian.PutUint32(rhdr[8:], uint32(encH264))
	buf.Write(rhdr[:])

	var body [8]byte
	binary.BigEndian.PutUint32(body[0:], uint32(len(data)))
	binary.BigEndian.PutUint32(body[4:], flags)
	buf.Write(body[:])
	buf.Write(data)

	if err := c.writeFull(buf.Bytes()); err != nil {
		c.logf("h264 update write: %v", err)
		c.close()
		return false
	}
	return true
}

func (c *client) handleKeyEvent(down bool, keysym uint32) {
	if c.server.config.HID == nil {
		return
	}
	code, needsShift, ok := hid.KeysymToHID(keysym)
	if !ok {
		return
	}
	if c.pressedKeys == nil {
		c.pressedKeys = make(map[uint8]bool)
	}

	if down {
		if !c.pressedKeys[code] {
			c.pressedKeys[code] = true
			// Force the Shift modifier for characters that require it when the
			// client did not send a Shift key itself (e.g. '!', '@', 'A').
			shiftInjected := false
			if needsShift && !c.pressedKeys[0xE1] && !c.pressedKeys[0xE5] {
				c.server.config.HID.KeyDown(0xE1)
				shiftInjected = true
			}
			c.server.config.HID.KeyDown(code)
			if shiftInjected {
				c.server.config.HID.KeyRelease(0xE1)
			}
		}
	} else {
		delete(c.pressedKeys, code)
		c.server.config.HID.KeyRelease(code)
	}
}

// vncBtnToHID maps VNC button bits (0=left, 1=middle, 2=right) to HID button
// numbers (1=left, 2=right, 3=middle).
var vncBtnToHID = [3]int{1, 3, 2}

func (c *client) handlePointerEvent(buttons uint8, x, y uint16) {
	if c.server.config.HID == nil {
		return
	}
	oldX, oldY := c.mouseX, c.mouseY
	c.mouseX = int(x)
	c.mouseY = int(y)

	// Pre-declare absolute-mode variables so the goto at end of relative
	// block doesn't jump over their declaration.
	hidX := int(x) * 32767 / int(c.width)
	hidY := int(y) * 32767 / int(c.height)

	if c.mouseRelative && c.qemuMouseRel {
		// QEMU -263 relative mouse mode: TigerVNC/noVNC have grabbed the
		// mouse, warped it to center, and send PointerEvents relative to the
		// framebuffer center. Compute the delta and forward it as-is.
		cx := int(c.width) / 2
		cy := int(c.height) / 2
		dx := int(x) - cx
		dy := int(y) - cy

		// Detect ungrab (Escape release): coordinates far from center mean
		// the client has released the grab.
		threshold := (int(c.width) + int(c.height)) / 8
		if abs(dx) > threshold || abs(dy) > threshold {
			c.mouseRelative = false
			log.Printf("vnc: client left relative mouse mode (detected ungrab)")
		} else {
			if oldX != int(x) || oldY != int(y) {
				c.server.config.HID.MouseMoveRel(dx, dy)
			}
			goto buttons
		}
	}

	// Absolute path (default, or fallback after ungrab detection).
	hidX = int(x) * 32767 / int(c.width)
	hidY = int(y) * 32767 / int(c.height)
	if oldX != int(x) || oldY != int(y) {
		c.server.config.HID.MouseMove(hidX, hidY)
	}

buttons:
	for b := 0; b < 3; b++ {
		mask := uint8(1) << b
		wasDown := (c.lastButtons & mask) != 0
		isDown := (buttons & mask) != 0
		if wasDown != isDown {
			c.server.config.HID.MouseButton(vncBtnToHID[b], isDown)
		}
	}

	// Wheel up (bit3) / down (bit4): fire one step on the press edge.
	if buttons&0x08 != 0 && c.lastButtons&0x08 == 0 {
		c.server.config.HID.MouseWheel(1)
	}
	if buttons&0x10 != 0 && c.lastButtons&0x10 == 0 {
		c.server.config.HID.MouseWheel(-1)
	}

	c.lastButtons = buttons
}

func (c *client) sendQEMUMouseMode(relative bool) {
	var state uint8
	if relative {
		state = 1
	}
	var buf [4]byte
	buf[0] = msgQEMU
	buf[1] = submsgQEMUPtrMot
	buf[2] = state
	// buf[3] already 0

	c.writeMu.Lock()
	_, err := c.conn.Write(buf[:])
	c.writeMu.Unlock()
	if err != nil {
		c.logf("QEMU mouse mode %s: %v", map[bool]string{true: "relative", false: "absolute"}[relative], err)
	}
	c.mouseRelative = relative
	if relative {
		c.mouseX, c.mouseY = int(c.width/2), int(c.height/2)
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
