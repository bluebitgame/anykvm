package wsmedia

import (
	"anykvm/internal/framebuf"
	"anykvm/internal/videotiming"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type wsClient struct {
	conn         *websocket.Conn
	srv          *Server
	sendCh       chan *outboundMessage
	sendMu       sync.RWMutex
	done         chan struct{}
	once         sync.Once
	active       bool            // guarded by Server.mu; capability handshake has started capture.
	audioCodecs  map[string]bool // guarded by Server.mu
	audioFormats map[string]bool // nil for legacy codec-only capability messages

	// JPEG pictures are independent: retain only the newest unsent picture.
	// Keep this separate from ordered video, metadata and audio messages.
	jpegMu    sync.Mutex
	jpeg      *outboundMessage
	jpegReady chan struct{}

	// input state (accessed only from readLoop)
	pressed              map[uint8]bool    // currently-pressed HID usages
	lastButtons          uint8             // last pointer button mask
	lastX, lastY         int               // last pointer position (framebuffer pixels)
	positionSet          bool              // the first event must move even at (0, 0)
	lastKeyframeRequest  time.Time         // accessed only from readLoop
	lastVideoDiagnostics time.Time         // accessed only from readLoop
	pointerStats         pointerInputStats // accessed only from readLoop
}

type pointerInputStats struct {
	started, last time.Time
	events        int
	maxGap        time.Duration
	maxHID        time.Duration
}

// outboundMessage keeps a small protocol header separate from immutable media.
// The queue owns buffer until the write completes, or the message is discarded.
type outboundMessage struct {
	timing videotiming.Delivery
	header []byte
	data   []byte
	buffer *framebuf.Buffer
}

func (m *outboundMessage) release() {
	if m != nil {
		m.buffer.Release()
	}
}
func (m *outboundMessage) kind() byte {
	if len(m.header) != 0 {
		return m.header[0]
	}
	if len(m.data) != 0 {
		return m.data[0]
	}
	return 0
}
func (c *wsClient) trySend(msg []byte) {
	c.enqueue(&outboundMessage{data: msg}, false, nil)
}

// enqueue consumes the message's reference on every path. Serializing admission
// with close prevents a send selected concurrently with cancellation from leaking.
func (c *wsClient) enqueue(msg *outboundMessage, ordered bool, stop <-chan struct{}) {
	msg.timing = msg.timing.Enqueue()
	c.sendMu.RLock()
	defer c.sendMu.RUnlock()
	select {
	case <-c.done:
		msg.release()
		return
	default:
	}
	select {
	case <-stop:
		msg.release()
		return
	default:
	}
	if msg.kind() == msgJPEG {
		c.jpegMu.Lock()
		c.jpeg.release()
		c.jpeg = msg
		select {
		case c.jpegReady <- struct{}{}:
		default:
		}
		c.jpegMu.Unlock()
		return
	}
	if ordered {
		c.clearJPEG()
		select {
		case c.sendCh <- msg:
			return
		case <-c.done:
		case <-stop:
		}
	} else {
		select {
		case c.sendCh <- msg:
			return
		default:
		}
	}
	msg.release()
}

func (c *wsClient) takeJPEG() *outboundMessage {
	c.jpegMu.Lock()
	defer c.jpegMu.Unlock()
	msg := c.jpeg
	c.jpeg = nil
	return msg
}
func (c *wsClient) clearJPEG() {
	c.jpegMu.Lock()
	defer c.jpegMu.Unlock()
	c.jpeg.release()
	c.jpeg = nil
	select {
	case <-c.jpegReady:
	default:
	}
}

func (c *wsClient) writeLoop() {
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	defer c.close()
	for {
		var msg *outboundMessage
		select {
		case <-c.done:
			return
		default:
		}
		// Metadata precedes pictures whose dimensions depend on it.
		select {
		case msg = <-c.sendCh:
		default:
			select {
			case <-c.done:
				return
			case msg = <-c.sendCh:
			case <-c.jpegReady:
				msg = c.takeJPEG()
			case <-ping.C:
				if !c.writeMessage(websocket.PingMessage, nil) {
					return
				}
				continue
			}
		}
		if msg != nil && !c.writeOutbound(msg) {
			return
		}
	}
}
func (c *wsClient) writeOutbound(msg *outboundMessage) bool {
	defer msg.release()
	var started time.Time
	if msg.timing.Trace != nil {
		started = time.Now()
	}
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	w, err := c.conn.NextWriter(websocket.BinaryMessage)
	if err == nil {
		if len(msg.header) != 0 {
			_, err = w.Write(msg.header)
		}
		if err == nil {
			_, err = w.Write(msg.data)
		}
		closeErr := w.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err != nil {
		c.close()
		return false
	}
	msg.timing.Sent("websocket", started)
	return true
}
func (c *wsClient) writeMessage(kind int, msg []byte) bool {
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := c.conn.WriteMessage(kind, msg); err != nil {
		c.close()
		return false
	}
	return true
}

// readLoop dispatches inbound input messages (keyboard/mouse/hello) to the HID
// device via the server; unknown types are ignored.
func (c *wsClient) readLoop() {
	c.conn.SetReadLimit(4096)
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			c.close()
			return
		}
		if len(data) == 0 {
			continue
		}
		c.srv.handleInput(c, data)
	}
}

func (c *wsClient) close() {
	c.once.Do(func() {
		close(c.done)
		if c.conn != nil {
			c.conn.Close()
		}
		c.sendMu.Lock()
		defer c.sendMu.Unlock()
		c.clearJPEG()
		for {
			select {
			case msg := <-c.sendCh:
				msg.release()
			default:
				return
			}
		}
	})
}
