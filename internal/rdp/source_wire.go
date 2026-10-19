package rdp

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"anykvm/internal/capture"
	"anykvm/internal/videodec"
)

type sourceFragments struct {
	total int
	data  []byte
}

func (f *sourceFragments) append(total int, first, last bool, p []byte) ([]byte, error) {
	if total < 1 || total > maxMessage {
		return nil, errProtocol
	}
	if first {
		if f.total != 0 {
			return nil, errProtocol
		}
		f.total = total
		f.data = f.data[:0]
	}
	if f.total != total || len(p) > total-len(f.data) {
		return nil, errProtocol
	}
	f.data = append(f.data, p...)
	if !last {
		return nil, nil
	}
	if len(f.data) != total {
		return nil, errProtocol
	}
	f.total = 0
	return f.data, nil
}

type sourceSurface struct {
	w, h    int
	pixels  []byte
	decoder videodec.Decoder
}
type sourceSession struct {
	conn                             net.Conn
	opts                             SourceOptions
	timeout                          time.Duration
	writeMu, mu, inputMu             sync.Mutex
	done, finished, ready            chan struct{}
	once, readyOnce                  sync.Once
	err                              error
	frames                           chan *capture.Frame
	audio                            chan *capture.AudioFrame
	epoch                            uint64
	decoder                          videodec.Decoder
	user, channel, dvc, audioChannel uint16
	share                            uint32
	width, height                    int
	vc                               map[uint16]*sourceFragments
	dynamic                          sourceFragments
	gfxChannelID                     uint32
	dvcVersion                       uint16
	lite                             rdp8LiteDecoder
	zgfx                             zgfxDecoder
	surfaces                         map[uint16]*sourceSurface
	mapped                           uint16
	mappedOK                         bool
	originX, originY                 int
	frameID, decodedFrames           uint32
	dirty                            bool
	inFrame                          bool
	keys                             [256]bool
	buttons                          [4]bool
	mouseX, mouseY                   int
	audioFormat                      int
	audioFormats                     bool
	wavePending                      []byte
	waveStamp                        uint16
	waveBlock                        byte
	waveSize                         int
}

func (s *sourceSession) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return fmt.Errorf("%w: RDP: %v", capture.ErrStreamStopped, s.err)
	}
	return capture.ErrStreamStopped
}
func (s *sourceSession) stop(e error) {
	s.once.Do(func() { s.mu.Lock(); s.err = e; s.mu.Unlock(); close(s.done); s.conn.Close() })
}
func (s *sourceSession) write(p []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.conn.SetWriteDeadline(time.Now().Add(s.timeout))
	for len(p) > 0 {
		n, e := s.conn.Write(p)
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}
func (s *sourceSession) sendMCS(ch uint16, p []byte) error {
	if len(p) > 32767 {
		return fmt.Errorf("RDP source MCS packet too large")
	}
	h := []byte{2, 0xf0, 0x80, 0x64, 0, 0, 0, 0, 0x70}
	be.PutUint16(h[4:], s.user-1001)
	be.PutUint16(h[6:], ch)
	return s.write(tpkt(join(h, perLength(len(p)), p)))
}
func (s *sourceSession) packet(p []byte) error { return s.write(tpkt(join([]byte{2, 0xf0, 0x80}, p))) }
func (s *sourceSession) readSlow() ([]byte, error) {
	p, fast, e := readPacket(s.conn)
	if e != nil {
		return nil, e
	}
	if fast {
		return nil, fmt.Errorf("unexpected fast-path packet during RDP connection")
	}
	return p, nil
}
func (s *sourceSession) handshake(address, password string, w, h int) error {
	requested := protocolHybrid
	if s.opts.Security == "tls" {
		requested = protocolTLS
	}
	request := []byte{14, 0xe0, 0, 0, 0, 0, 0, 1, 0, 8, 0, 0, 0, 0, 0}
	le.PutUint32(request[11:], requested)
	if e := s.write(tpkt(request)); e != nil {
		return e
	}
	p, e := s.readSlow()
	if e != nil {
		return e
	}
	if len(p) != 15 || p[1] != 0xd0 || p[7] != 2 || le.Uint32(p[11:]) != requested {
		return fmt.Errorf("RDP server rejected requested %s security", s.opts.Security)
	}
	host, _, _ := net.SplitHostPort(address)
	conn := tls.Client(s.conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, InsecureSkipVerify: s.opts.InsecureSkipVerify})
	s.conn = conn
	if e = conn.Handshake(); e != nil {
		return e
	}
	if requested == protocolHybrid {
		if e = s.sourceNLA(conn, password); e != nil {
			return e
		}
	}
	if e = s.packet(sourceConnect(w, h, requested)); e != nil {
		return e
	}
	p, e = s.readSlow()
	if e != nil {
		return e
	}
	if e = s.connectResponse(p, requested); e != nil {
		return e
	}
	if e = s.packet([]byte{4, 1, 0, 1, 0}); e != nil {
		return e
	}
	if e = s.packet([]byte{0x28}); e != nil {
		return e
	}
	p, e = s.readSlow()
	if e != nil {
		return e
	}
	if len(p) != 7 || !bytes.Equal(p[:5], []byte{2, 0xf0, 0x80, 0x2e, 0}) {
		return errProtocol
	}
	s.user = be.Uint16(p[5:]) + 1001
	for _, ch := range []uint16{s.user, s.channel, s.dvc, s.audioChannel} {
		if ch == 0 {
			continue
		}
		if e = s.packet(join([]byte{0x38}, []byte{byte((s.user - 1001) >> 8), byte(s.user - 1001), byte(ch >> 8), byte(ch)})); e != nil {
			return e
		}
		p, e = s.readSlow()
		if e != nil {
			return e
		}
		if len(p) != 11 || p[3] != 0x3e || p[4] != 0 || be.Uint16(p[7:]) != ch || be.Uint16(p[9:]) != ch {
			return errProtocol
		}
	}
	return s.sendMCS(s.channel, sourceInfo(s.opts.Username, s.opts.Domain, password))
}
func sourceConnect(w, h int, selected uint32) []byte {
	core := make([]byte, 230)
	le.PutUint32(core, 0x80004)
	le.PutUint16(core[4:], uint16(w))
	le.PutUint16(core[6:], uint16(h))
	le.PutUint16(core[8:], 0xca01)
	le.PutUint16(core[10:], 0xaa03)
	le.PutUint32(core[12:], 0x409)
	le.PutUint32(core[16:], 26000)
	copy(core[20:], utf16Bytes("AnyKVM"))
	le.PutUint32(core[52:], 4)
	le.PutUint32(core[60:], 12)
	le.PutUint16(core[128:], 0xca01)
	le.PutUint16(core[130:], 1)
	le.PutUint16(core[136:], 24)
	le.PutUint16(core[138:], 0xf)
	le.PutUint16(core[140:], 0x102)
	core[206] = 6
	le.PutUint32(core[208:], selected)
	le.PutUint32(core[222:], 100)
	le.PutUint32(core[226:], 100)
	channels := join(u32(2), []byte("drdynvc\x00"), u32(0xc0000000), []byte("rdpsnd\x00\x00"), u32(0xc0000000))
	blocks := join(capability(0xc001, core), capability(0xc002, make([]byte, 8)), capability(0xc003, channels))
	gcc := join([]byte{0, 5, 0, 0x14, 0x7c, 0, 1}, perLength(len(blocks)+14), []byte{0, 8, 0, 0x10, 0, 1, 0xc0, 0}, []byte("Duca"), perLength(len(blocks)), blocks)
	domain := func(values ...int) []byte {
		var p []byte
		for _, n := range values {
			p = append(p, berInt(n)...)
		}
		return berValue([]byte{0x30}, p)
	}
	body := join(berValue([]byte{4}, []byte{1}), berValue([]byte{4}, []byte{1}), []byte{1, 1, 0xff}, domain(34, 2, 0, 1, 0, 1, 65535, 2), domain(1, 1, 1, 1, 0, 1, 1056, 2), domain(65535, 64535, 65535, 1, 0, 1, 65535, 2), berValue([]byte{4}, gcc))
	return berValue([]byte{0x7f, 0x65}, body)
}
func (s *sourceSession) connectResponse(p []byte, requested uint32) error {
	if len(p) < 3 || !bytes.Equal(p[:3], []byte{2, 0xf0, 0x80}) {
		return errProtocol
	}
	v, rest, e := takeBER(p[3:], []byte{0x7f, 0x66})
	if e != nil || len(rest) != 0 {
		return errProtocol
	}
	result, v, e := takeBER(v, []byte{10})
	if e != nil || len(result) != 1 || result[0] != 0 {
		return fmt.Errorf("RDP MCS connect rejected")
	}
	for _, tag := range []byte{2, 0x30} {
		_, v, e = takeBER(v, []byte{tag})
		if e != nil {
			return e
		}
	}
	gcc, rest, e := takeBER(v, []byte{4})
	if e != nil || len(rest) != 0 {
		return errProtocol
	}
	idx := bytes.Index(gcc, []byte("McDn"))
	if idx < 0 {
		return errProtocol
	}
	n, blocks, e := takePer(gcc[idx+4:])
	if e != nil || n != len(blocks) {
		return errProtocol
	}
	for len(blocks) > 0 {
		if len(blocks) < 4 {
			return errProtocol
		}
		kind, size := le.Uint16(blocks), int(le.Uint16(blocks[2:]))
		if size < 4 || size > len(blocks) {
			return errProtocol
		}
		b := blocks[4:size]
		switch kind {
		case 0xc01:
			if len(b) < 8 || le.Uint32(b[4:]) != requested {
				return errProtocol
			}
		case 0xc02:
			if len(b) < 8 || le.Uint32(b) != 0 || le.Uint32(b[4:]) != 0 {
				return fmt.Errorf("RDP legacy encryption unsupported")
			}
		case 0xc03:
			if len(b) < 8 || le.Uint16(b[2:]) != 2 {
				return errProtocol
			}
			s.channel, s.dvc, s.audioChannel = le.Uint16(b), le.Uint16(b[4:]), le.Uint16(b[6:])
		}
		blocks = blocks[size:]
	}
	if s.channel == 0 || s.dvc == 0 || s.audioChannel == 0 || s.channel == s.dvc || s.dvc == s.audioChannel {
		return errProtocol
	}
	return nil
}
func sourceInfo(user, domain, password string) []byte {
	p := join(u32(0x40), u32(0), u32(0x0001017b))
	for _, v := range []string{domain, user, password, "", ""} {
		p = append(p, u16(len(utf16Bytes(v)))...)
	}
	for _, v := range []string{domain, user, password, "", ""} {
		p = append(p, utf16Bytes(v)...)
		p = append(p, 0, 0)
	}
	// Extended Info: IPv4 address, client directory, UTC timezone, session ID,
	// performance flags and an empty auto-reconnect cookie.
	address, directory := utf16Bytes("127.0.0.1\x00"), utf16Bytes("C:\\AnyKVM\x00")
	return join(p, u16(2), u16(len(address)), address, u16(len(directory)), directory, make([]byte, 172), u32(0), u32(0), u16(0), u16(0), u16(0))
}
func (s *sourceSession) shareControl(kind int, p []byte) []byte {
	return join(u16(len(p)+6), u16(0x10|kind), u16(int(s.user)), p)
}
func (s *sourceSession) shareData(kind byte, p []byte) []byte {
	return s.shareControl(7, join(u32(s.share), []byte{0, 1}, u16(12+len(p)), []byte{kind, 0, 0, 0}, p))
}
func (s *sourceSession) activate(p []byte) error {
	if len(p) < 18 {
		return errProtocol
	}
	s.share = le.Uint32(p[6:])
	descLen, capsLen := int(le.Uint16(p[10:])), int(le.Uint16(p[12:]))
	off := 14 + descLen
	if capsLen < 4 || off+capsLen > len(p) {
		return errProtocol
	}
	// Use the same capability layouts as the server, with drawing orders,
	// bitmap caching and fast-path output disabled. RDPGFX carries all video.
	general := make([]byte, 20)
	le.PutUint16(general[4:], 0x200)
	le.PutUint16(general[10:], 0x404)
	bitmap := make([]byte, 24)
	le.PutUint16(bitmap, 32)
	le.PutUint16(bitmap[2:], 1)
	le.PutUint16(bitmap[4:], 1)
	le.PutUint16(bitmap[6:], 1)
	le.PutUint16(bitmap[8:], uint16(s.width))
	le.PutUint16(bitmap[10:], uint16(s.height))
	le.PutUint16(bitmap[16:], 1)
	bitmap[19] = 0x0e
	order := make([]byte, 84)
	le.PutUint16(order[30:], 2)
	input := make([]byte, 84)
	le.PutUint16(input, 0x11)
	le.PutUint32(input[4:], 0x409)
	le.PutUint32(input[8:], 4)
	le.PutUint32(input[16:], 12)
	caps := join(capability(1, general), capability(2, bitmap), capability(3, order), capability(8, join(u16(0), u16(0))), capability(13, input), capability(20, join(u32(0), u32(vcChunk))), capability(9, join(u16(int(s.user)), u16(0))), capability(12, make([]byte, 4)))
	desc := []byte("AnyKVM\x00")
	confirm := s.shareControl(3, join(u32(s.share), u16(1002), u16(len(desc)), u16(4+len(caps)), desc, u16(8), u16(0), caps))
	if e := s.sendMCS(s.channel, confirm); e != nil {
		return e
	}
	for _, p := range [][]byte{s.shareData(31, join(u16(1), u16(1001))), s.shareData(20, join(u16(4), make([]byte, 6))), s.shareData(20, join(u16(1), make([]byte, 6))), s.shareData(39, join(u16(0), u16(0), u16(3), u16(0x32)))} {
		if e := s.sendMCS(s.channel, p); e != nil {
			return e
		}
	}
	return nil
}
func (s *sourceSession) run() {
	defer close(s.finished)
	defer func() {
		if s.decoder != nil {
			s.decoder.Close()
		}
	}()
	defer func() {
		for _, surface := range s.surfaces {
			if surface.decoder != nil && surface.decoder != s.decoder {
				surface.decoder.Close()
			}
		}
	}()
	for {
		p, fast, e := readPacket(s.conn)
		if e != nil {
			s.stop(e)
			return
		}
		if fast { // No fast-path output was advertised.
			s.stop(fmt.Errorf("unexpected RDP fast-path output"))
			return
		}
		if len(p) < 10 || !bytes.Equal(p[:3], []byte{2, 0xf0, 0x80}) || p[3] != 0x68 {
			s.stop(errProtocol)
			return
		}
		ch := be.Uint16(p[6:])
		n, p, e := takePer(p[9:])
		if e != nil || n != len(p) {
			s.stop(errProtocol)
			return
		}
		if ch == s.channel {
			e = s.handleSourceShare(p)
		} else if ch == s.dvc || ch == s.audioChannel {
			e = s.handleSourceVC(ch, p)
		} else {
			e = fmt.Errorf("unexpected RDP channel")
		}
		if e != nil {
			s.stop(e)
			return
		}
	}
}
func (s *sourceSession) handleSourceShare(p []byte) error {
	// Licensing preamble precedes Share Control until VALID_CLIENT is received.
	if len(p) >= 4 && le.Uint32(p) == 0x80 {
		if len(p) < 8 || p[4] != 0xff || len(p) < 20 || le.Uint32(p[8:]) != 7 {
			return fmt.Errorf("RDP licensing exchange is unsupported (server must accept a valid client)")
		}
		return nil
	}
	for len(p) > 0 {
		if len(p) < 6 {
			return errProtocol
		}
		n := int(le.Uint16(p))
		if n < 6 || n > len(p) {
			return errProtocol
		}
		msg := p[:n]
		p = p[n:]
		switch le.Uint16(msg[2:]) & 15 {
		case 1:
			if e := s.activate(msg); e != nil {
				return e
			}
		case 6:
			s.mappedOK = false
		case 7:
			if len(msg) < 18 || msg[15] != 0 {
				return errProtocol
			}
			switch msg[14] {
			case 2:
				return fmt.Errorf("RDP source requires H.264/AVC420; server sent legacy graphics")
			case 47:
				if len(msg) < 22 {
					return errProtocol
				}
				if code := le.Uint32(msg[18:]); code != 0 {
					return fmt.Errorf("RDP server error %#x", code)
				}
			case 36:
				return io.EOF
			}
		default:
			return fmt.Errorf("unsupported RDP share message")
		}
	}
	return nil
}
func (s *sourceSession) sendVC(ch uint16, p []byte) error {
	for off := 0; off < len(p); {
		n := min(vcChunk, len(p)-off)
		flags := uint32(0)
		if off == 0 {
			flags |= 1
		}
		if off+n == len(p) {
			flags |= 2
		}
		if e := s.sendMCS(ch, join(u32(uint32(len(p))), u32(flags), p[off:off+n])); e != nil {
			return e
		}
		off += n
	}
	return nil
}
func (s *sourceSession) handleSourceVC(ch uint16, p []byte) error {
	if len(p) < 8 {
		return errProtocol
	}
	total, flags := int(le.Uint32(p)), le.Uint32(p[4:])
	if flags&^uint32(0x13) != 0 {
		return errProtocol
	}
	f := s.vc[ch]
	if f == nil {
		f = &sourceFragments{}
		s.vc[ch] = f
	}
	body, e := f.append(total, flags&1 != 0, flags&2 != 0, p[8:])
	if e != nil || body == nil {
		return e
	}
	if ch == s.dvc {
		return s.handleSourceDVC(body)
	} // Legacy WaveInfo declares the length of both channel messages.
	if s.wavePending == nil && len(body) == 16 && body[0] == soundWave {
		if !s.audioFormats || s.audioFormat < 0 || le.Uint16(body[6:]) != 0 || int(le.Uint16(body[2:])) < 12 {
			return errProtocol
		}
		s.waveStamp = le.Uint16(body[4:])
		s.waveBlock = body[8]
		s.waveSize = int(le.Uint16(body[2:])) - 8
		s.wavePending = append([]byte(nil), body[12:16]...)
		return nil
	}
	return s.handleSourceAudio(body)
}
