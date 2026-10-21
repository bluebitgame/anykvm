package vnc

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"

	"anykvm/internal/capture"
)

func (c *client) sendCursorIfChanged() bool {
	cs, ok := c.captureCursorState()
	if !ok {
		return false
	}
	enc, data, w, h, hotX, hotY := c.prepareCursor(cs)
	if enc == 0 {
		return false
	}
	if !c.curLogged {
		c.logf("cursor: %dx%d hot=(%d,%d) size=%d enc=%s", w, h, hotX, hotY, len(data), encodingName(enc))
		c.curLogged = true
	}

	var buf bytes.Buffer
	buf.WriteByte(msgFramebufferUpdate)
	buf.WriteByte(0)
	var nr [2]byte
	binary.BigEndian.PutUint16(nr[:], 1)
	buf.Write(nr[:])

	c.writeCursorRectHeader(&buf, enc, w, h, hotX, hotY)
	buf.Write(data)

	if err := c.writeFull(buf.Bytes()); err != nil {
		c.logf("sendCursorUpdate write: %v", err)
		c.close()
		return false
	}
	return true
}

func (c *client) captureCursorState() (*capture.CursorState, bool) {
	if c.cursorCap == nil {
		return nil, false
	}
	cs, err := c.cursorCap.Capture()
	if err != nil || cs == nil {
		return nil, false
	}
	enc, data, w, h, x, y := c.prepareCursor(cs)
	if enc == 0 {
		return nil, false
	}
	hash := sha256.New()
	var header [12]byte
	binary.BigEndian.PutUint32(header[:], uint32(enc))
	binary.BigEndian.PutUint16(header[4:], w)
	binary.BigEndian.PutUint16(header[6:], h)
	binary.BigEndian.PutUint16(header[8:], x)
	binary.BigEndian.PutUint16(header[10:], y)
	hash.Write(header[:])
	hash.Write(data)
	var sum [32]byte
	copy(sum[:], hash.Sum(nil))
	// Changed is a source-wide hint. Every viewer needs its own initial shape
	// and must notice hide/default states even if another viewer polled first.
	if c.cursorSent && c.cursorHash == sum {
		return nil, false
	}
	c.cursorHash, c.cursorSent = sum, true
	return cs, true
}

func (c *client) writeCursorRectHeader(buf *bytes.Buffer, enc int32, w, h, hotX, hotY uint16) {
	var rhdr [12]byte
	binary.BigEndian.PutUint16(rhdr[0:], hotX)
	binary.BigEndian.PutUint16(rhdr[2:], hotY)
	binary.BigEndian.PutUint16(rhdr[4:], w)
	binary.BigEndian.PutUint16(rhdr[6:], h)
	binary.BigEndian.PutUint32(rhdr[8:], uint32(enc))
	buf.Write(rhdr[:])
}
