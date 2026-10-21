package rdp

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

const gfxChannel = 1
const dvcMaxVersion uint16 = 3

type displayCodec uint16

const (
	codecClear       displayCodec = 0x0008
	codecProgressive displayCodec = 0x0009
	codecPlanar      displayCodec = 0x000a
	codecAVC420      displayCodec = 0x000b
)

func (codec displayCodec) String() string {
	switch codec {
	case codecAVC420:
		return "h264"
	case codecPlanar:
		return "planar"
	case codecProgressive:
		return "progressive"
	default:
		return "clearcodec"
	}
}

func (c *client) sendVC(p []byte) error {
	return c.sendStaticVC(c.data.dvc, p)
}

func (c *client) sendStaticVC(channel uint16, p []byte) error {
	c.staticWriteMu.Lock()
	defer c.staticWriteMu.Unlock()
	return c.sendStaticVCLocked(channel, p)
}

// Caller holds staticWriteMu.
func (c *client) sendStaticVCLocked(channel uint16, p []byte) error {
	for off := 0; off < len(p); {
		n := min(vcChunk, len(p)-off)
		flags := uint32(0)
		if off == 0 {
			flags |= 1
		}
		if off+n == len(p) {
			flags |= 2
		}
		if err := c.sendMCS(channel, join(u32(uint32(len(p))), u32(flags), p[off:off+n])); err != nil {
			return err
		}
		off += n
	}
	return nil
}

func (c *client) handleVC(p []byte) error {
	if !c.dvcCaps {
		return fmt.Errorf("%w: static-channel data before DRDYNVC initialization", errProtocol)
	}
	if len(p) < 8 {
		return fmt.Errorf("%w: static-channel header length %d", errProtocol, len(p))
	}
	total, flags := int(le.Uint32(p)), le.Uint32(p[4:])
	p = p[8:]
	if total < 1 || total > 1<<20 {
		return fmt.Errorf("%w: static-channel total length %d", errProtocol, total)
	}
	if flags & ^uint32(0x13) != 0 {
		return fmt.Errorf("%w: unsupported static-channel flags %#x", errProtocol, flags)
	}
	if flags&1 != 0 {
		if c.vcTotal != 0 {
			return fmt.Errorf("%w: overlapping static-channel fragments", errProtocol)
		}
		c.vcTotal = total
		c.vcBuf = c.vcBuf[:0]
	}
	if c.vcTotal != total || len(p) > total-len(c.vcBuf) {
		return fmt.Errorf("%w: static-channel fragment length %d, total %d, buffered %d, expected total %d", errProtocol, len(p), total, len(c.vcBuf), c.vcTotal)
	}
	c.vcBuf = append(c.vcBuf, p...)
	if flags&2 == 0 {
		return nil
	}
	if len(c.vcBuf) != total {
		return fmt.Errorf("%w: incomplete static-channel message (%d of %d bytes)", errProtocol, len(c.vcBuf), total)
	}
	c.vcTotal = 0
	if err := c.handleDVC(c.vcBuf); err != nil {
		return fmt.Errorf("static-channel flags=%#x length=%d: %w", flags, total, err)
	}
	return nil
}

func dvcInteger(p []byte, width byte) (uint32, []byte, error) {
	if width > 2 {
		return 0, nil, errProtocol
	}
	n := 1 << width
	if len(p) < n {
		return 0, nil, errProtocol
	}
	v := uint32(0)
	for i := 0; i < n; i++ {
		v |= uint32(p[i]) << (8 * i)
	}
	return v, p[n:], nil
}

func (c *client) handleDVC(p []byte) error {
	if len(p) == 0 {
		return fmt.Errorf("%w: empty DVC message", errProtocol)
	}
	header := p[0]
	p = p[1:]
	cmd := header >> 4
	if (cmd == 6 || cmd == 7) && len(p)+1 > vcChunk {
		return fmt.Errorf("%w: compressed DVC PDU exceeds 1600 bytes", errProtocol)
	}
	if cmd == 5 {
		if len(p) != 3 || c.gfxCreated {
			return fmt.Errorf("%w: DVC capabilities length %d, repeated=%t", errProtocol, len(p)+1, c.gfxCreated)
		}
		version := le.Uint16(p[1:])
		if version < 1 || version > dvcMaxVersion {
			return fmt.Errorf("%w: unsupported DVC capability version %d", errProtocol, version)
		}
		// MS-RDPEDYC 1.7: the response advertises the client's maximum version.
		// Select the highest version supported by both endpoints.
		c.dvcClientVersion = version
		c.dvcVersion = min(version, dvcMaxVersion)
		c.gfxCreated = true
		if err := c.sendVC(join([]byte{0x10, gfxChannel}, []byte("Microsoft::Windows::RDS::Graphics\x00"))); err != nil {
			return err
		}
		if c.s.cfg.OnDesktopResize != nil {
			c.dispCreated = true
			return c.sendVC(join([]byte{0x10, dispChannel}, []byte("Microsoft::Windows::RDS::DisplayControl\x00")))
		}
		return nil
	}
	ch, p, err := dvcInteger(p, header&3)
	if err != nil {
		return fmt.Errorf("DVC command=%#x channel-id width=%d: %w", cmd, header&3, err)
	}
	if ch == dispChannel {
		return c.handleDisplayDVC(header, p)
	}
	if ch != gfxChannel || !c.gfxCreated {
		return fmt.Errorf("%w: DVC command=%#x channel=%d, graphics requested=%t", errProtocol, cmd, ch, c.gfxCreated)
	}
	switch cmd {
	case 1:
		if len(p) != 4 || c.gfxOpen {
			return fmt.Errorf("%w: DVC create-response length %d, repeated=%t", errProtocol, len(p), c.gfxOpen)
		}
		if status := le.Uint32(p); int32(status) < 0 {
			return fmt.Errorf("client rejected the RDPGFX channel (HRESULT=%#08x)", status)
		}
		c.gfxOpen = true
	case 2, 6:
		if !c.gfxOpen {
			return fmt.Errorf("%w: DVC data-first before create response", errProtocol)
		}
		n, data, err := dvcInteger(p, (header>>2)&3)
		if err != nil || n == 0 || n > 1<<20 || c.dvcTotal != 0 {
			return fmt.Errorf("%w: DVC data-first total=%d, length width=%d, pending total=%d", errProtocol, n, (header>>2)&3, c.dvcTotal)
		}
		if cmd == 6 {
			if c.dvcVersion != 3 {
				return fmt.Errorf("%w: compressed DVC data without version 3 negotiation", errProtocol)
			}
			data, err = c.dvcCompression.decode(data)
			if err != nil {
				return err
			}
		}
		c.dvcTotal, c.dvcBuf = int(n), c.dvcBuf[:0]
		return c.appendGFX(data)
	case 3, 7:
		if !c.gfxOpen {
			return fmt.Errorf("%w: DVC data before create response", errProtocol)
		}
		if cmd == 7 {
			if c.dvcVersion != 3 {
				return fmt.Errorf("%w: compressed DVC data without version 3 negotiation", errProtocol)
			}
			p, err = c.dvcCompression.decode(p)
			if err != nil {
				return err
			}
		}
		if c.dvcTotal == 0 {
			return c.handleGFX(p)
		}
		return c.appendGFX(p)
	case 4:
		return errors.New("client closed the RDPGFX channel")
	default:
		return fmt.Errorf("%w: unsupported DVC command=%#x header=%#x", errProtocol, cmd, header)
	}
	return nil
}

func (c *client) appendGFX(p []byte) error {
	if len(p) > c.dvcTotal-len(c.dvcBuf) {
		return fmt.Errorf("%w: DVC fragment length %d, buffered %d, total %d", errProtocol, len(p), len(c.dvcBuf), c.dvcTotal)
	}
	c.dvcBuf = append(c.dvcBuf, p...)
	if len(c.dvcBuf) != c.dvcTotal {
		return nil
	}
	c.dvcTotal = 0
	return c.handleGFX(c.dvcBuf)
}

func avcCapability(p []byte) (uint32, uint32, error) {
	version, flags, _, err := graphicsCapability(p, "h264")
	return version, flags, err
}

func graphicsCapability(p []byte, preference string) (uint32, uint32, displayCodec, error) {
	return graphicsCapabilityWithReport(p, preference, nil)
}

func graphicsCapabilityWithReport(p []byte, preference string, report func(string), policies ...*CodecOptions) (uint32, uint32, displayCodec, error) {
	allowed := DefaultCodecOptions()
	if len(policies) != 0 && policies[0] != nil {
		allowed = *policies[0]
	}
	if len(p) < 2 {
		return 0, 0, 0, fmt.Errorf("%w: truncated GFX capability count", errProtocol)
	}
	count := int(le.Uint16(p))
	p = p[2:]
	if count < 1 || count > 64 {
		return 0, 0, 0, fmt.Errorf("%w: GFX capability count %d", errProtocol, count)
	}
	var version, flags uint32
	var bitmapVersion, bitmapFlags uint32
	var progressiveVersion, progressiveFlags uint32
	advertised := make([]string, 0, count)
	for i := 0; i < count; i++ {
		if len(p) < 8 {
			return 0, 0, 0, fmt.Errorf("%w: truncated GFX capability %d", errProtocol, i)
		}
		v, size := le.Uint32(p), int(le.Uint32(p[4:]))
		if size < 4 || size > len(p)-8 {
			return 0, 0, 0, fmt.Errorf("%w: GFX capability %d version=%#x data length=%d, available=%d", errProtocol, i, v, size, len(p)-8)
		}
		f := le.Uint32(p[8:])
		supported := false
		bitmap := false
		reason := "unknown_version"
		switch v {
		case 0x80004:
			bitmap = size == 4
		case 0x80105:
			bitmap = size == 4
			supported = bitmap && f&0x10 != 0
			reason = "AVC420_ENABLED_absent"
		case 0xa0100:
			// Version 10.1 implies AVC support and has 16 reserved zero bytes,
			// not a flags word (MS-RDPEGFX 2.2.3.4 and 1.7).
			supported = size == 16 && bytes.Equal(p[8:8+size], make([]byte, 16))
			bitmap = supported
			reason = "invalid_v10.1_reserved_data"
		case 0xa0002, 0xa0200, 0xa0301, 0xa0400, 0xa0502, 0xa0600, 0xa0601, 0xa0701:
			// 0xa0601 is the historical documented spelling of version 10.6.
			bitmap = size == 4
			supported = bitmap && f&0x20 == 0
			reason = "AVC_DISABLED"
		}
		if supported {
			reason = "enabled"
		}
		advertised = append(advertised, fmt.Sprintf("version=0x%08x length=%d flags=0x%08x AVC420=%t (%s)", v, size, f, supported, reason))
		scaledFlags := uint32(0)
		if v == 0xa0701 {
			scaledFlags = f & gfxCapsScaledMapDisable
		}
		if supported && v > version {
			version, flags = v, scaledFlags
			if v == 0x80105 {
				flags = 0x10
			}
		}
		if bitmap && v > bitmapVersion {
			bitmapVersion, bitmapFlags = v, f&3|scaledFlags
			if v >= 0xa0002 && v != 0xa0100 {
				bitmapFlags |= 0x20
			}
		}
		// THINCLIENT peers only require regular RemoteFX. Do not clear their
		// flag to force a codec that they did not promise to implement.
		if bitmap && f&1 == 0 && v > progressiveVersion {
			progressiveVersion, progressiveFlags = v, f&2|scaledFlags
			if v >= 0xa0002 && v != 0xa0100 {
				progressiveFlags |= 0x20
			}
		}
		p = p[8+size:]
	}
	if len(p) != 0 {
		return 0, 0, 0, fmt.Errorf("%w: %d trailing GFX capability bytes", errProtocol, len(p))
	}
	if report != nil {
		report(strings.Join(advertised, "; "))
	}
	automatic := preference == "" || preference == "auto"
	if allowed.H264 && (automatic || preference == "h264") && version != 0 {
		return version, flags, codecAVC420, nil
	}
	if preference == "h264" {
		return 0, 0, 0, fmt.Errorf("no usable AVC420 capability advertised; H.264 is required; capabilities: [%s]", strings.Join(advertised, "; "))
	}
	// Automatic selection prefers AVC420, then eligible Progressive, then
	// Planar. Planar and ClearCodec share the baseline capability requirement;
	// explicit policies keep their requested codec.
	if allowed.Progressive && automatic && progressiveVersion != 0 {
		return progressiveVersion, progressiveFlags, codecProgressive, nil
	}
	if preference == "progressive" {
		if !allowed.Progressive || progressiveVersion == 0 {
			return 0, 0, 0, fmt.Errorf("no usable non-THINCLIENT capability advertised; RemoteFX Progressive is required; capabilities: [%s]", strings.Join(advertised, "; "))
		}
		return progressiveVersion, progressiveFlags, codecProgressive, nil
	}
	if bitmapVersion == 0 {
		return 0, 0, 0, fmt.Errorf("no implemented RDPGFX capability advertised; capabilities: [%s]", strings.Join(advertised, "; "))
	}
	if allowed.Planar && (automatic || preference == "planar") {
		return bitmapVersion, bitmapFlags, codecPlanar, nil
	}
	if allowed.ClearCodec && (automatic || preference == "clearcodec") {
		return bitmapVersion, bitmapFlags, codecClear, nil
	}
	return 0, 0, 0, fmt.Errorf("no permitted RDPGFX encoding is supported by this client; allowed: [%s]", allowed)
}

func (c *client) handleGFX(p []byte) error {
	for len(p) > 0 {
		if len(p) < 8 {
			return fmt.Errorf("%w: GFX header length %d", errProtocol, len(p))
		}
		kind, n := le.Uint16(p), int(le.Uint32(p[4:]))
		if n < 8 || n > len(p) || le.Uint16(p[2:]) != 0 {
			return fmt.Errorf("%w: GFX command=%#x length=%d, available=%d, flags=%#x", errProtocol, kind, n, len(p), le.Uint16(p[2:]))
		}
		body := p[8:n]
		p = p[n:]
		switch kind {
		case 0x12:
			if c.ready {
				return fmt.Errorf("%w: repeated GFX capability advertisement", errProtocol)
			}
			runtime := c.s.runtimeConfig()
			allowed := DefaultCodecOptions()
			if runtime.AllowedCodecs != nil {
				allowed = *runtime.AllowedCodecs
			}
			version, flags, codec, err := graphicsCapabilityWithReport(body, runtime.Codec, func(advertised string) {
				log.Printf("rdp: %s: RDPGFX capabilities advertised: [%s]", c.conn.RemoteAddr(), advertised)
			}, runtime.AllowedCodecs)
			if err != nil {
				return err
			}
			if err := c.sendGFX(gfxCapabilityConfirm(version, flags)); err != nil {
				return err
			}
			log.Printf("rdp: %s: RDPGFX capability confirmed version=0x%08x flags=0x%08x codec=%s allowed=[%s]", c.conn.RemoteAddr(), version, flags, codec, allowed)
			c.codec = codec
			c.scaledMapping = graphicsSupportsScaledMapping(version, flags)
			if err := c.activate(); err != nil {
				return err
			}
		case 0xd:
			if !c.ready || len(body) != 12 {
				return fmt.Errorf("%w: GFX frame-ack length %d, ready=%t", errProtocol, len(body), c.ready)
			}
			frame := le.Uint32(body[4:])
			c.ackMu.Lock()
			if frame > c.sent {
				c.ackMu.Unlock()
				return errors.New("acknowledgment for an unsent frame")
			}
			if frame > c.acknowledged {
				c.acknowledged = frame
			}
			c.suspendAck = le.Uint32(body) == 0xffffffff
			c.ackMu.Unlock()
			select {
			case c.ack <- struct{}{}:
			default:
			}
		case 0x16:
			if !c.ready || len(body) != 12 {
				return fmt.Errorf("%w: GFX QoE frame-ack length %d, ready=%t", errProtocol, len(body), c.ready)
			}
		case 0x10:
			// Clients may advertise cached surfaces; this server does not reuse them.
			if len(body) < 2 || len(body) != 2+int(le.Uint16(body))*12 {
				return fmt.Errorf("%w: GFX cache-import-offer length %d", errProtocol, len(body))
			}
		default:
			return fmt.Errorf("%w: unsupported GFX command=%#x length=%d", errProtocol, kind, n)
		}
	}
	return nil
}

func gfxPDU(kind int, p []byte) []byte { return join(u16(kind), u16(0), u32(uint32(8+len(p))), p) }

func gfxCapabilityConfirm(version, flags uint32) []byte {
	data := u32(flags)
	if version == 0xa0100 {
		data = make([]byte, 16)
	}
	return gfxPDU(0x13, join(u32(version), u32(uint32(len(data))), data))
}

// Display codecs already compress their bitmap payloads. Transport them using
// valid uncompressed RDP8 segments without additional bulk compression.
func segmentGFX(p []byte) []byte {
	const chunk = 65535
	if len(p) <= chunk {
		return join([]byte{0xe0, 4}, p)
	}
	n := (len(p) + chunk - 1) / chunk
	out := join([]byte{0xe1}, u16(n), u32(uint32(len(p))))
	for len(p) > 0 {
		k := min(chunk, len(p))
		out = append(out, u32(uint32(k+1))...)
		out = append(out, 4)
		out = append(out, p[:k]...)
		p = p[k:]
	}
	return out
}

func (c *client) sendGFX(p []byte) error {
	if len(p) > maxMessage {
		return errors.New("graphics message too large")
	}
	p = segmentGFX(p)
	if len(p) <= vcChunk-2 {
		return c.sendVC(join([]byte{0x30, gfxChannel}, p))
	}
	first := true
	for len(p) > 0 {
		h := []byte{0x30, gfxChannel}
		if first {
			h = join([]byte{0x28, gfxChannel}, u32(uint32(len(p))))
			first = false
		}
		n := min(vcChunk-len(h), len(p))
		if err := c.sendVC(join(h, p[:n])); err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

func (c *client) ensureGraphicsSize(w, h int) error {
	return c.ensureGraphicsLayout(makeGraphicsLayout(w, h, w, h, false))
}

func graphicsReset(w, h int) []byte {
	return join(graphicsResetOutput(w, h), graphicsCreateSurface(1, w, h), gfxPDU(0xf, join(u16(1), u16(0), u32(0), u32(0))))
}

func graphicsResetOutput(w, h int) []byte {
	p := make([]byte, 332)
	le.PutUint32(p, uint32(w))
	le.PutUint32(p[4:], uint32(h))
	le.PutUint32(p[8:], 1)
	le.PutUint32(p[20:], uint32(w-1))
	le.PutUint32(p[24:], uint32(h-1))
	le.PutUint32(p[28:], 1)
	return gfxPDU(0xe, p)
}

func graphicsFrame(data []byte, w, h int, id, timestamp uint32) []byte {
	rect := join(u16(0), u16(0), u16(w), u16(h))
	avc := join(u32(1), rect, []byte{22, 100}, data)
	wire := join(u16(1), u16(0xb), []byte{0x20}, rect, u32(uint32(len(avc))), avc)
	return join(gfxPDU(0xb, join(u32(timestamp), u32(id))), gfxPDU(1, wire), gfxPDU(0xc, u32(id)))
}

func (c *client) frameWindowFull() bool {
	c.ackMu.Lock()
	defer c.ackMu.Unlock()
	return !c.suspendAck && c.sent-c.acknowledged >= 2
}

func (c *client) waitWindow() error {
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		if !c.frameWindowFull() {
			return nil
		}
		if timer == nil {
			timer = time.NewTimer(5 * time.Second)
		}
		select {
		case <-c.done:
			return nil
		case <-c.ack:
		case <-timer.C:
			return errors.New("graphics frame acknowledgment timed out")
		}
	}
}
