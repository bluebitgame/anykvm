package vnc

import (
	"encoding/binary"
	"io"

	"anykvm/internal/capture"
)

func (c *client) setQEMUAudioCapability(enabled bool) {
	c.audioMu.Lock()
	defer c.audioMu.Unlock()
	if !enabled {
		c.stopQEMUAudioLocked()
		c.audioFormat = qemuAudioFormat{}
	}
	c.audioSupported = enabled
	if enabled {
		// FramebufferUpdate with one payload-free capability rectangle.
		var ack [16]byte
		ack[3] = 1
		enc := int32(encQEMUAudio)
		binary.BigEndian.PutUint32(ack[12:], uint32(enc))
		if err := c.writeFull(ack[:]); err != nil {
			c.logf("QEMU audio: capability: %v", err)
		}
	}
}

func (c *client) handleQEMUAudio() {
	var op [2]byte
	if _, err := io.ReadFull(c.conn, op[:]); err != nil {
		return
	}
	operation := binary.BigEndian.Uint16(op[:])
	var format qemuAudioFormat
	if operation == qemuAudioSetFmt {
		var buf [6]byte
		if _, err := io.ReadFull(c.conn, buf[:]); err != nil {
			return
		}
		format = qemuAudioFormat{
			sampleFormat: buf[0],
			channels:     buf[1],
			sampleRate:   binary.BigEndian.Uint32(buf[2:]),
		}
	}
	c.audioMu.Lock()
	defer c.audioMu.Unlock()
	if !c.audioSupported {
		return
	}
	switch operation {
	case qemuAudioSetFmt:
		// Format changes end the old stream. The client must enable the new one.
		c.stopQEMUAudioLocked()
		c.audioFormat = format
		if !format.valid() {
			c.logf("QEMU audio: unsupported format %d/%d/%d; audio disabled", format.sampleFormat, format.channels, format.sampleRate)
		}
	case qemuAudioEnable:
		if !c.audioActive && c.audioFormat.valid() {
			c.audioConverter = qemuAudioConverter{}
			c.sendQEMUAudioStart()
			c.audioActive = true
		}
	case qemuAudioDisable:
		c.stopQEMUAudioLocked()
	}
}

// stopQEMUAudioLocked requires audioMu. End precedes any subsequent stream data.
func (c *client) stopQEMUAudioLocked() {
	if c.audioActive {
		c.sendQEMUAudioEnd()
	}
	c.audioActive = false
	c.audioConverter = qemuAudioConverter{}
}

func (c *client) pushQEMUAudio(a capture.AudioFrame) {
	c.audioMu.Lock()
	defer c.audioMu.Unlock()
	if !c.audioSupported || !c.audioActive {
		return
	}
	if pcm := c.audioConverter.convert(a, c.audioFormat); len(pcm) > 0 {
		c.sendQEMUAudioData(pcm)
	}
}

func (c *client) sendQEMUAudioStart() {
	var buf [4]byte
	buf[0] = msgQEMU
	buf[1] = submsgQEMUAudio
	binary.BigEndian.PutUint16(buf[2:], qemuAudioStart)
	c.writeMu.Lock()
	_, err := c.conn.Write(buf[:])
	c.writeMu.Unlock()
	if err != nil {
		c.logf("QEMU audio: send start: %v", err)
	}
}

func (c *client) sendQEMUAudioEnd() {
	var buf [4]byte
	buf[0] = msgQEMU
	buf[1] = submsgQEMUAudio
	binary.BigEndian.PutUint16(buf[2:], qemuAudioEnd)
	c.writeMu.Lock()
	_, err := c.conn.Write(buf[:])
	c.writeMu.Unlock()
	if err != nil {
		c.logf("QEMU audio: send end: %v", err)
	}
}

func (c *client) sendQEMUAudioData(pcm []byte) {
	// Header: msg(1) + sub(1) + op(2) + dataLen(4) = 8 bytes
	header := make([]byte, 8)
	header[0] = msgQEMU
	header[1] = submsgQEMUAudio
	binary.BigEndian.PutUint16(header[2:], qemuAudioData)
	binary.BigEndian.PutUint32(header[4:], uint32(len(pcm)))

	err := c.writeFull(header, pcm)
	if err != nil {
		c.logf("QEMU audio: send data: %v", err)
	}
}
