package vnc

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"anykvm/internal/capture"
	"anykvm/internal/wol"
)

func IsVNCDevice(deviceID string) bool {
	return strings.HasPrefix(deviceID, "vnc://")
}

// Bound allocations driven by the remote server before reading payloads.
const (
	maxVNCFramePixels = 16 << 20
	maxVNCPayload     = 64 << 20
	maxVNCName        = 1 << 20
)

type VNCClient struct {
	wakeOnLAN wol.Options
	host      string
	port      int
	password  string

	conn   net.Conn
	reader io.Reader

	fb       []byte
	fbWidth  int
	fbHeight int
	fbName   string

	pf pixelFormat

	mu          sync.Mutex
	running     bool
	frameCh     chan *capture.Frame
	closed      chan struct{}
	workers     sync.WaitGroup
	lifecycleMu sync.Mutex
	inputMu     sync.Mutex

	pressed    map[uint8]uint32
	lastMouseX int
	lastMouseY int
	lastBtn    uint8
	writeMu    sync.Mutex

	mouseRelative bool

	audioCb         func([]byte)
	audioBuf        []byte
	audioMu         sync.Mutex
	audioNegotiated bool // owned by recvLoop, reset before starting it
	audioStarted    bool // owned by recvLoop
}

func NewVNCClient(deviceID string) (*VNCClient, error) {
	raw := strings.TrimPrefix(deviceID, "vnc://")
	host, port, password := parseVNCHost(raw)
	if host == "" {
		return nil, fmt.Errorf("vnc: invalid device id %q", deviceID)
	}
	return &VNCClient{
		host:     host,
		port:     port,
		password: password,
		frameCh:  make(chan *capture.Frame, 2),
		closed:   make(chan struct{}),
	}, nil
}

func parseVNCHost(raw string) (host string, port int, password string) {
	if strings.Contains(raw, "@") {
		parts := strings.SplitN(raw, "@", 2)
		password = parts[0]
		raw = parts[1]
	}
	if u, err := url.Parse("vnc://" + raw); err == nil {
		host = u.Hostname()
		if p := u.Port(); p != "" {
			fmt.Sscanf(p, "%d", &port)
		}
		if pw := u.Query().Get("password"); pw != "" {
			password = pw
		}
	}
	if host == "" {
		if i := strings.LastIndexByte(raw, ':'); i >= 0 {
			host = raw[:i]
			fmt.Sscanf(raw[i+1:], "%d", &port)
		} else {
			host = raw
		}
	}
	if port == 0 {
		port = 5900
	}
	return
}

// SetWakeOnLAN configures waking before the first Open.
func (c *VNCClient) SetWakeOnLAN(options wol.Options) { c.wakeOnLAN = options }

func (c *VNCClient) Open() error {
	return c.OpenWithFormat(0, 0, 0)
}

func (c *VNCClient) OpenWithFormat(width, height, fps int) error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	c.mu.Lock()
	running := c.running
	c.mu.Unlock()
	if running {
		return nil
	}
	c.workers.Wait()
	c.closed = make(chan struct{})
	c.frameCh = make(chan *capture.Frame, 2)
	c.pressed = make(map[uint8]uint32)
	c.lastMouseX, c.lastMouseY, c.lastBtn = 0, 0, 0
	c.mouseRelative = false
	c.audioNegotiated, c.audioStarted = false, false
	c.audioMu.Lock()
	c.audioBuf = nil
	c.audioMu.Unlock()
	addr := net.JoinHostPort(c.host, strconv.Itoa(c.port))
	var err error
	c.conn, err = c.wakeOnLAN.DialContext(context.Background(), addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("vnc client: connect %s: %w", addr, err)
	}
	c.reader = c.conn
	c.conn.SetDeadline(time.Now().Add(10 * time.Second))

	if err := c.handshake(); err != nil {
		c.conn.Close()
		return err
	}

	if err := c.sendSetPixelFormat(); err != nil {
		c.conn.Close()
		return fmt.Errorf("vnc: set pixel format: %w", err)
	}
	// Compressed encodings require stream state that this client does not yet
	// decode completely. Request only encodings with exact payload framing.
	if err := c.sendSetEncodings([]int32{
		encCopyRect, encRaw, encDesktopSize,
		encQEMUAudio, encQEMUPointerMotion,
	}); err != nil {
		c.conn.Close()
		return fmt.Errorf("vnc: set encodings: %w", err)
	}

	c.conn.SetDeadline(time.Time{})

	c.mu.Lock()
	c.running = true
	c.mu.Unlock()
	c.workers.Add(2)
	go c.sendUpdateRequests()
	go c.recvLoop()
	return nil
}

func (c *VNCClient) handshake() error {
	buf := make([]byte, 24)
	if _, err := io.ReadFull(c.reader, buf[:12]); err != nil {
		return fmt.Errorf("vnc: read server version: %w", err)
	}
	serverVer := string(buf[:12])
	log.Printf("vnc client: server version %q", strings.TrimSpace(serverVer))

	if _, err := io.WriteString(c.conn, protoVersion); err != nil {
		return fmt.Errorf("vnc: write client version: %w", err)
	}

	if _, err := io.ReadFull(c.reader, buf[:1]); err != nil {
		return fmt.Errorf("vnc: read security type count: %w", err)
	}
	nSec := int(buf[0])
	secTypes := make([]byte, nSec)
	if nSec > 0 {
		if _, err := io.ReadFull(c.reader, secTypes); err != nil {
			return fmt.Errorf("vnc: read security types: %w", err)
		}
	}

	if nSec == 0 {
		reason, err := c.readSizedPayload(maxVNCName)
		if err != nil {
			return err
		}
		return fmt.Errorf("vnc: server rejected connection: %s", reason)
	}

	chosen := c.selectAuth(secTypes)
	buf[0] = chosen
	if _, err := c.conn.Write(buf[:1]); err != nil {
		return fmt.Errorf("vnc: write chosen auth: %w", err)
	}

	if err := c.doAuth(chosen); err != nil {
		return err
	}

	buf[0] = 1
	if _, err := c.conn.Write(buf[:1]); err != nil {
		return fmt.Errorf("vnc: write ClientInit: %w", err)
	}

	if _, err := io.ReadFull(c.reader, buf[:24]); err != nil {
		return fmt.Errorf("vnc: read ServerInit: %w", err)
	}
	c.fbWidth = int(readU16BE(buf, 0))
	c.fbHeight = int(readU16BE(buf, 2))
	c.pf = parsePixelFormat(buf[4:20])

	nameLen := int(readU32BE(buf, 20))
	if nameLen > maxVNCName {
		return fmt.Errorf("vnc: desktop name too large: %d", nameLen)
	}
	if nameLen > 0 {
		nameBuf := make([]byte, nameLen)
		if _, err := io.ReadFull(c.reader, nameBuf); err != nil {
			return fmt.Errorf("vnc: read desktop name: %w", err)
		}
		c.fbName = string(nameBuf)
	}

	log.Printf("vnc client: desktop %dx%d %q", c.fbWidth, c.fbHeight, c.fbName)
	if !validVNCSize(c.fbWidth, c.fbHeight) {
		return fmt.Errorf("vnc: invalid desktop size %dx%d", c.fbWidth, c.fbHeight)
	}
	c.fb = make([]byte, c.fbWidth*c.fbHeight*4)

	return nil
}

func (c *VNCClient) selectAuth(types []byte) byte {
	hasTLS := false
	hasVNC := false
	hasNone := false
	for _, t := range types {
		switch t {
		case secTypeVeNCrypt:
			hasTLS = true
		case secTypeVNC:
			hasVNC = true
		case secTypeNone:
			hasNone = true
		}
	}
	if hasTLS {
		return secTypeVeNCrypt
	}
	if c.password != "" && hasVNC {
		return secTypeVNC
	}
	if hasNone {
		return secTypeNone
	}
	return types[0]
}

func (c *VNCClient) doAuth(t byte) error {
	switch t {
	case secTypeNone:
		return c.authNone()
	case secTypeVNC:
		return c.authVNC()
	case secTypeVeNCrypt:
		return c.authVeNCrypt()
	default:
		return fmt.Errorf("vnc: unsupported auth type %d", t)
	}
}

func (c *VNCClient) authNone() error {
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c.reader, buf); err != nil {
		return fmt.Errorf("vnc: read auth none result: %w", err)
	}
	if readU32BE(buf, 0) != 0 {
		return fmt.Errorf("vnc: auth none failed")
	}
	return nil
}

func (c *VNCClient) authVNC() error {
	challenge := make([]byte, 16)
	if _, err := io.ReadFull(c.reader, challenge); err != nil {
		return fmt.Errorf("vnc: read challenge: %w", err)
	}
	response, err := vncEncrypt(challenge, c.password)
	if err != nil {
		return fmt.Errorf("vnc: encrypt challenge: %w", err)
	}
	if _, err := c.conn.Write(response); err != nil {
		return fmt.Errorf("vnc: write auth response: %w", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c.reader, buf); err != nil {
		return fmt.Errorf("vnc: read auth result: %w", err)
	}
	if readU32BE(buf, 0) != 0 {
		return fmt.Errorf("vnc: auth failed")
	}
	return nil
}

func (c *VNCClient) authVeNCrypt() error {
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c.reader, buf[:2]); err != nil {
		return fmt.Errorf("vnc: read vencrypt version: %w", err)
	}
	major, minor := buf[0], buf[1]
	log.Printf("vnc client: VeNCrypt version %d.%d", major, minor)

	buf[0], buf[1] = major, minor
	if _, err := c.conn.Write(buf[:2]); err != nil {
		return fmt.Errorf("vnc: write vencrypt version: %w", err)
	}

	if _, err := io.ReadFull(c.reader, buf[:1]); err != nil {
		return fmt.Errorf("vnc: read vencrypt ack: %w", err)
	}

	if buf[0] != 0 {
		return fmt.Errorf("vnc: VeNCrypt version rejected")
	}
	if _, err := io.ReadFull(c.reader, buf[:1]); err != nil {
		return fmt.Errorf("vnc: read vencrypt subtype count: %w", err)
	}
	nSub := int(buf[0])
	subtypes := make([]byte, 4*nSub)
	if _, err := io.ReadFull(c.reader, subtypes); err != nil {
		return fmt.Errorf("vnc: read vencrypt subtypes: %w", err)
	}

	var chosen uint32
	for i := 0; i < nSub; i++ {
		st := binary.BigEndian.Uint32(subtypes[i*4:])
		if st == 260 || st == 261 {
			chosen = st
			break
		}
	}
	if chosen == 0 {
		return fmt.Errorf("vnc: no supported VeNCrypt subtype")
	}

	binary.BigEndian.PutUint32(buf[:4], chosen)
	if _, err := c.conn.Write(buf[:4]); err != nil {
		return fmt.Errorf("vnc: write vencrypt subtype: %w", err)
	}

	if _, err := io.ReadFull(c.reader, buf[:1]); err != nil {
		return fmt.Errorf("vnc: read VeNCrypt subtype ack: %w", err)
	}
	if buf[0] != 1 {
		return fmt.Errorf("vnc: VeNCrypt subtype rejected")
	}
	tlsConn := tls.Client(c.conn, &tls.Config{InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err != nil {
		return fmt.Errorf("vnc: TLS handshake: %w", err)
	}
	c.conn = tlsConn
	c.reader = tlsConn
	log.Printf("vnc client: VeNCrypt TLS established")

	if chosen == vencryptX509Vnc {
		return c.authVNC()
	}
	return c.authNone()
}

func (c *VNCClient) sendSetPixelFormat() error {
	buf := make([]byte, 20)
	buf[0] = msgSetPixelFormat
	buf[1], buf[2], buf[3] = 0, 0, 0

	buf[4] = 32
	buf[5] = 24
	buf[6] = 0
	buf[7] = 1
	binary.BigEndian.PutUint16(buf[8:10], 255)
	binary.BigEndian.PutUint16(buf[10:12], 255)
	binary.BigEndian.PutUint16(buf[12:14], 255)
	buf[14] = 16
	buf[15] = 8
	buf[16] = 0
	buf[17], buf[18], buf[19] = 0, 0, 0

	if _, err := c.conn.Write(buf); err != nil {
		return err
	}
	c.pf = defaultPixelFormat()
	return nil
}

func (c *VNCClient) sendSetEncodings(encodings []int32) error {
	buf := make([]byte, 4+4*len(encodings))
	buf[0] = msgSetEncodings
	buf[1] = 0
	binary.BigEndian.PutUint16(buf[2:4], uint16(len(encodings)))
	for i, e := range encodings {
		binary.BigEndian.PutUint32(buf[4+i*4:], uint32(e))
	}
	_, err := c.conn.Write(buf)
	return err
}

func (c *VNCClient) sendUpdateRequests() {
	defer c.workers.Done()
	if err := c.sendUpdateRequest(false); err != nil {
		c.conn.Close()
		return
	}

	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := c.sendUpdateRequest(true); err != nil {
				c.conn.Close()
				return
			}
		case <-c.closed:
			return
		}
	}
}

func (c *VNCClient) sendUpdateRequest(incremental bool) error {
	buf := make([]byte, 10)
	buf[0] = msgFramebufferUpdateRequest
	if incremental {
		buf[1] = 1
	}
	binary.BigEndian.PutUint16(buf[2:4], 0)
	binary.BigEndian.PutUint16(buf[4:6], 0)
	c.mu.Lock()
	binary.BigEndian.PutUint16(buf[6:8], uint16(c.fbWidth))
	binary.BigEndian.PutUint16(buf[8:10], uint16(c.fbHeight))
	c.mu.Unlock()

	return c.writeMessage(buf)
}

func (c *VNCClient) recvLoop() {
	defer c.workers.Done()
	defer func() {
		c.conn.Close()
		c.mu.Lock()
		c.running = false
		select {
		case <-c.closed:
		default:
			close(c.closed)
		}
		c.mu.Unlock()
	}()
	var message [1]byte
	for {
		if _, err := io.ReadFull(c.reader, message[:]); err != nil {
			return
		}
		var err error
		switch message[0] {
		case msgFramebufferUpdate:
			err = c.handleFramebufferUpdate()
		case msgQEMU:
			err = c.handleQEMUMessage()
		case 2: // Bell has no payload.
		case 3: // ServerCutText: padding, length, text.
			var padding [3]byte
			if _, err = io.ReadFull(c.reader, padding[:]); err == nil {
				_, err = c.readSizedPayload(maxVNCPayload)
			}
		default:
			err = fmt.Errorf("vnc: unsupported server message %d", message[0])
		}
		if err != nil {
			log.Printf("vnc client: %v", err)
			return
		}
	}
}

func (c *VNCClient) handleQEMUMessage() error {
	var sub [1]byte
	if _, err := io.ReadFull(c.reader, sub[:]); err != nil {
		return err
	}
	switch sub[0] {
	case submsgQEMUPtrMot:
		var state [2]byte // state plus padding, matching sendQEMUMouseMode
		if _, err := io.ReadFull(c.reader, state[:]); err != nil {
			return err
		}
		c.inputMu.Lock()
		c.mouseRelative = state[0] == 1
		c.inputMu.Unlock()
	case submsgQEMUAudio:
		return c.handleQEMUAudioMessage()
	default:
		return fmt.Errorf("vnc: unsupported QEMU message %d", sub[0])
	}
	return nil
}

func (c *VNCClient) readSizedPayload(limit int) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(c.reader, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if uint64(size) > uint64(limit) {
		return nil, fmt.Errorf("vnc: payload too large: %d", size)
	}
	data := make([]byte, int(size))
	_, err := io.ReadFull(c.reader, data)
	return data, err
}

func validVNCSize(w, h int) bool {
	return w > 0 && h > 0 && w <= 65535 && h <= 65535 && w <= maxVNCFramePixels/h
}

func (c *VNCClient) handleFramebufferUpdate() error {
	var header [3]byte
	if _, err := io.ReadFull(c.reader, header[:]); err != nil {
		return err
	}
	nRects := int(binary.BigEndian.Uint16(header[1:]))
	updated := false
	for i := 0; i < nRects; i++ {
		var rh [12]byte
		if _, err := io.ReadFull(c.reader, rh[:]); err != nil {
			return err
		}
		r := rect{x: readU16BE(rh[:], 0), y: readU16BE(rh[:], 2),
			width: readU16BE(rh[:], 4), height: readU16BE(rh[:], 6),
			encoding: int32(readU32BE(rh[:], 8))}
		if r.encoding == encQEMUAudio {
			// This pseudo-rectangle has no payload; QEMU may use desktop dimensions.
			if err := c.negotiateQEMUAudio(); err != nil {
				return err
			}
			continue
		}
		if r.encoding == encDesktopSize {
			if !validVNCSize(int(r.width), int(r.height)) {
				return fmt.Errorf("vnc: invalid desktop size %dx%d", r.width, r.height)
			}
			updated = true
			c.mu.Lock()
			c.fbWidth, c.fbHeight = int(r.width), int(r.height)
			c.fb = make([]byte, c.fbWidth*c.fbHeight*4)
			c.mu.Unlock()
			continue // DesktopSize dimensions are in the rectangle header; no payload.
		}
		c.mu.Lock()
		w, h := c.fbWidth, c.fbHeight
		c.mu.Unlock()
		if r.width == 0 || r.height == 0 || int(r.x)+int(r.width) > w || int(r.y)+int(r.height) > h {
			return fmt.Errorf("vnc: rectangle outside framebuffer")
		}
		var data []byte
		var err error
		switch r.encoding {
		case encRaw, encCopyRect:
			size := int(r.width) * int(r.height) * c.pf.bpp
			if r.encoding == encCopyRect {
				size = 4
			}
			data = make([]byte, size)
			_, err = io.ReadFull(c.reader, data)
		default:
			return fmt.Errorf("vnc: unsupported rectangle encoding %d", r.encoding)
		}
		if err != nil {
			return err
		}
		updated = true
		c.mu.Lock()
		switch r.encoding {
		case encRaw:
			decodeRawClient(c.fb, w, r, data, c.pf)
		case encCopyRect:
			srcX, srcY := int(readU16BE(data, 0)), int(readU16BE(data, 2))
			if srcX+int(r.width) > w || srcY+int(r.height) > h {
				err = fmt.Errorf("vnc: CopyRect source outside framebuffer")
			} else {
				decodeCopyRectClient(c.fb, w, r, data)
			}
		}
		c.mu.Unlock()
		if err != nil {
			return err
		}
	}
	if updated {
		c.mu.Lock()
		frame := &capture.Frame{Format: capture.FormatRaw,
			Data: append([]byte(nil), c.fb...), Width: c.fbWidth, Height: c.fbHeight}
		c.mu.Unlock()
		select {
		case c.frameCh <- frame:
		default:
		}
	}
	return nil
}

func (c *VNCClient) Capture() (*capture.Frame, error) {
	select {
	case frame := <-c.frameCh:
		return frame, nil
	case <-c.closed:
		return nil, fmt.Errorf("vnc: connection closed")
	}
}

func (c *VNCClient) Close() error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	c.mu.Lock()
	c.running = false
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	conn := c.conn
	c.mu.Unlock()
	var err error
	if conn != nil {
		err = conn.Close()
	}
	c.workers.Wait()
	return err
}

func (c *VNCClient) SetDecodeImage(bool) {}
func (c *VNCClient) SetJPEGQuality(int)  {}
func readU16BE(b []byte, off int) uint16 {
	if off+2 > len(b) {
		return 0
	}
	return binary.BigEndian.Uint16(b[off:])
}

func readU32BE(b []byte, off int) uint32 {
	if off+4 > len(b) {
		return 0
	}
	return binary.BigEndian.Uint32(b[off:])
}
