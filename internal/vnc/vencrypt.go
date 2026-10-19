package vnc

import (
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
)

// VeNCrypt security subtypes (RFC-less extension, see TigerVNC/VeNCrypt spec).
const (
	vencryptX509None = 260
	vencryptX509Vnc  = 261
)

// doVeNCrypt performs the VeNCrypt sub-handshake: version negotiation,
// subtype selection, TLS establishment using the server certificate, then the
// inner authentication (None or VNC Auth) over the encrypted channel.
func (c *client) doVeNCrypt() error {
	if _, err := c.conn.Write([]byte{0, 2}); err != nil {
		return fmt.Errorf("send VeNCrypt version: %w", err)
	}

	var ver [2]byte
	if _, err := io.ReadFull(c.conn, ver[:]); err != nil {
		return fmt.Errorf("read VeNCrypt version: %w", err)
	}
	if ver[0] != 0 || ver[1] < 2 {
		c.conn.Write([]byte{1})
		return fmt.Errorf("unsupported VeNCrypt version %d.%d", ver[0], ver[1])
	}
	if _, err := c.conn.Write([]byte{0}); err != nil {
		return fmt.Errorf("ack VeNCrypt version: %w", err)
	}

	var subtype uint32 = vencryptX509None
	if c.server.config.Password != "" {
		subtype = vencryptX509Vnc
	}
	var offer [5]byte
	offer[0] = 1
	binary.BigEndian.PutUint32(offer[1:], subtype)
	if _, err := c.conn.Write(offer[:]); err != nil {
		return fmt.Errorf("send VeNCrypt subtypes: %w", err)
	}

	var chosenBuf [4]byte
	if _, err := io.ReadFull(c.conn, chosenBuf[:]); err != nil {
		return fmt.Errorf("read VeNCrypt subtype: %w", err)
	}
	chosen := binary.BigEndian.Uint32(chosenBuf[:])
	if chosen != subtype {
		c.conn.Write([]byte{0})
		return fmt.Errorf("client chose unsupported VeNCrypt subtype %d", chosen)
	}
	if _, err := c.conn.Write([]byte{1}); err != nil {
		return fmt.Errorf("ack VeNCrypt subtype: %w", err)
	}

	tlsConn := tls.Server(c.conn, c.server.tlsConfig)
	if err := tlsConn.Handshake(); err != nil {
		return fmt.Errorf("VeNCrypt TLS handshake: %w", err)
	}
	c.conn = tlsConn
	c.logf("VeNCrypt TLS established (subtype %d)", chosen)

	switch chosen {
	case vencryptX509None:
		if _, err := c.conn.Write([]byte{0, 0, 0, secResultOK}); err != nil {
			return fmt.Errorf("send sec result: %w", err)
		}
	case vencryptX509Vnc:
		if err := c.doVNCAuth(); err != nil {
			return fmt.Errorf("vnc auth: %w", err)
		}
	}
	return nil
}
