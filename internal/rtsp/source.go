// Package rtsp implements H.264 RTSP sources without an FFmpeg dependency.
package rtsp

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"anykvm/internal/capture"
	"anykvm/internal/jpegcodec"
	"anykvm/internal/videodec"
)

type Options struct {
	Username    string        `yaml:"username,omitempty"`
	Password    string        `yaml:"password,omitempty"`
	PasswordEnv string        `yaml:"password_env,omitempty"`
	Transport   string        `yaml:"transport,omitempty"`
	Timeout     time.Duration `yaml:"timeout,omitempty"`
}

func IsDevice(s string) bool {
	s = strings.ToLower(s)
	return strings.HasPrefix(s, "rtsp:") || strings.HasPrefix(s, "rtsps:")
}
func ValidateSource(raw string, o Options) error {
	if !IsDevice(raw) {
		return nil
	}
	u, e := url.Parse(raw)
	if e != nil || u.Hostname() == "" || u.Fragment != "" || len(raw) > 16384 || strings.ContainsAny(raw, "\r\n\x00") || (u.Scheme != "rtsp" && u.Scheme != "rtsps") {
		return fmt.Errorf("RTSP requires rtsp://host/path or rtsps://host/path with optional user:password credentials")
	}
	if u.Port() != "" {
		p, e := strconv.Atoi(u.Port())
		if e != nil || p < 1 || p > 65535 {
			return fmt.Errorf("invalid RTSP port")
		}
	}
	if o.Transport != "" && o.Transport != "tcp" {
		return fmt.Errorf("rtsp.transport currently supports tcp only")
	}
	if o.Timeout < 0 || o.Timeout > 5*time.Minute {
		return fmt.Errorf("rtsp.timeout must be 0..5m (0 uses 10s)")
	}
	if o.Password != "" && o.PasswordEnv != "" {
		return fmt.Errorf("use only one of rtsp.password and password_env")
	}
	if strings.ContainsAny(o.Username+o.Password, "\r\n\x00") {
		return fmt.Errorf("invalid RTSP credentials")
	}
	return nil
}

type Device struct {
	raw                string
	opts               Options
	life, mu, decodeMu sync.Mutex
	s                  *session
	codec              string
	decode             bool
	quality            int
	decoder            videodec.Decoder
}
type session struct {
	conn                                   net.Conn
	r                                      *bufio.Reader
	writeMu                                sync.Mutex
	seq                                    int
	raw, password, session, auth, username string
	nonceCount                             int
	timeout                                time.Duration
	done, finished                         chan struct{}
	once                                   sync.Once
	frames                                 chan *capture.Frame
	audio                                  chan *capture.AudioFrame
	audioTrack                             *audioTrack
	mu                                     sync.Mutex
	err                                    error
	dep                                    h264RTP
}

func NewCapture(raw string, o Options) (capture.Device, bool) {
	if !IsDevice(raw) {
		return nil, false
	}
	if err := ValidateSource(raw, o); err != nil {
		return capture.Failed(err), true
	}
	u, _ := url.Parse(raw)
	if u.User != nil {
		if o.Username == "" {
			o.Username = u.User.Username()
		}
		if o.Password == "" && o.PasswordEnv == "" {
			o.Password, _ = u.User.Password()
		}
		u.User = nil
	}
	if strings.ContainsAny(o.Username+o.Password, "\r\n\x00") {
		return capture.Failed(fmt.Errorf("invalid RTSP credentials")), true
	}
	return &Device{raw: u.String(), opts: o}, true
}
func (d *Device) Open() error { return d.OpenWithFormat(0, 0, 0) }
func (d *Device) OpenWithFormat(_, _, _ int) error {
	d.life.Lock()
	defer d.life.Unlock()
	d.mu.Lock()
	old := d.s
	d.mu.Unlock()
	if old != nil {
		return fmt.Errorf("RTSP source is already open")
	}
	password := d.opts.Password
	if d.opts.PasswordEnv != "" {
		var ok bool
		password, ok = os.LookupEnv(d.opts.PasswordEnv)
		if !ok {
			return fmt.Errorf("rtsp.password_env is not set")
		}
	}
	u, _ := url.Parse(d.raw)
	port := u.Port()
	if port == "" {
		port = "554"
		if u.Scheme == "rtsps" {
			port = "322"
		}
	}
	timeout := d.opts.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return fmt.Errorf("RTSP connect: %w", err)
	}
	if u.Scheme == "rtsps" {
		secure := tls.Client(conn, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
		conn = secure
		if err = secure.HandshakeContext(ctx); err != nil {
			conn.Close()
			return fmt.Errorf("RTSP TLS: %w", err)
		}
	}
	s := &session{conn: conn, r: bufio.NewReaderSize(conn, 65536), raw: d.raw, password: password, username: d.opts.Username, audio: make(chan *capture.AudioFrame, 5), timeout: timeout, done: make(chan struct{}), finished: make(chan struct{}), frames: make(chan *capture.Frame, 16)}
	conn.SetDeadline(time.Now().Add(timeout))
	if err = d.setup(s); err != nil {
		conn.Close()
		return err
	}
	conn.SetDeadline(time.Time{})
	d.mu.Lock()
	d.s = s
	d.mu.Unlock()
	go s.run()
	return nil
}
func (d *Device) setup(s *session) error {
	response, err := s.request("DESCRIBE", s.raw, map[string]string{"Accept": "application/sdp"}, d.opts.Username)
	if err != nil {
		return err
	}
	base := s.raw
	if response.headers["content-base"] != "" {
		base = response.headers["content-base"]
	}
	track, aggregate, err := parseSDP(response.body, base)
	if err != nil {
		return err
	}
	s.dep = track.dep
	setup, err := s.request("SETUP", track.control, map[string]string{"Transport": "RTP/AVP/TCP;unicast;interleaved=0-1"}, d.opts.Username)
	if err != nil {
		return err
	}
	s.session = strings.TrimSpace(strings.Split(setup.headers["session"], ";")[0])
	if s.session == "" {
		return fmt.Errorf("RTSP SETUP omitted Session")
	}
	transport := strings.ToLower(setup.headers["transport"])
	if !strings.Contains(transport, "rtp/avp/tcp") || !strings.Contains(transport, "interleaved=0-1") {
		return fmt.Errorf("RTSP server did not accept TCP interleaved channels 0-1")
	}
	audio, err := sdpAudio(response.body, base)
	if err != nil {
		return err
	}
	if audio != nil {
		reply, e := s.request("SETUP", audio.control, map[string]string{"Transport": "RTP/AVP/TCP;unicast;interleaved=2-3"}, d.opts.Username)
		if e != nil {
			return fmt.Errorf("RTSP audio SETUP: %w", e)
		}
		if !strings.Contains(strings.ToLower(reply.headers["transport"]), "interleaved=2-3") || strings.Split(reply.headers["session"], ";")[0] != s.session {
			return fmt.Errorf("RTSP audio did not accept shared TCP session")
		}
		s.audioTrack = audio
	}
	_, err = s.request("PLAY", aggregate, nil, d.opts.Username)
	return err
}
func (s *session) stop(err error) {
	s.once.Do(func() { s.mu.Lock(); s.err = err; s.mu.Unlock(); close(s.done); s.conn.Close() })
}
func (s *session) run() {
	defer close(s.finished)
	// OPTIONS keeps the session alive while the single reader consumes both
	// interleaved media and replies. The CSeq writer is serialized.
	keepDone := make(chan struct{})
	defer close(keepDone)
	go func() {
		ticker := time.NewTicker(min(15*time.Second, max(time.Second, s.timeout/2)))
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := s.send("OPTIONS", s.raw, nil, s.username); err != nil {
					s.stop(err)
					return
				}
			case <-keepDone:
				return
			case <-s.done:
				return
			}
		}
	}()
	for {
		s.conn.SetReadDeadline(time.Now().Add(s.timeout))
		response, packet, ch, err := s.read()
		if err != nil {
			s.stop(err)
			return
		}
		if response != nil {
			if response.status/100 != 2 {
				s.stop(fmt.Errorf("RTSP keepalive status %d", response.status))
				return
			}
			continue
		}
		if ch == 2 && s.audioTrack != nil {
			frames, e := s.audioTrack.push(packet)
			if e != nil {
				s.stop(e)
				return
			}
			for _, f := range frames {
				select {
				case s.audio <- f:
				default:
					select {
					case <-s.audio:
					default:
					}
					select {
					case s.audio <- f:
					default:
					}
				}
			}
			continue
		}
		if ch != 0 {
			continue
		}
		data, w, h, err := s.dep.push(packet)
		if err != nil {
			s.stop(err)
			return
		}
		if len(data) == 0 {
			continue
		}
		f := &capture.Frame{Data: data, Format: capture.FormatH264, Width: w, Height: h, SourceWidth: w, SourceHeight: h, CapturedAt: time.Now()}
		select {
		case s.frames <- f:
		case <-s.done:
			return
		}
	}
}
func (d *Device) Capture() (*capture.Frame, error) {
	d.mu.Lock()
	s, codec, decode, q := d.s, d.codec, d.decode, d.quality
	d.mu.Unlock()
	if s == nil {
		return nil, capture.ErrStreamStopped
	}
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	var f *capture.Frame
	select {
	case <-s.done:
		s.mu.Lock()
		err := s.err
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: RTSP: %v", capture.ErrStreamStopped, err)
	case f = <-s.frames:
	case <-timer.C:
		return nil, nil
	}
	if codec == "h264" && !decode {
		return f, nil
	}
	d.decodeMu.Lock()
	defer d.decodeMu.Unlock()
	d.mu.Lock()
	current := d.s
	d.mu.Unlock()
	if current != s {
		return nil, capture.ErrStreamStopped
	}
	if d.decoder == nil {
		var err error
		d.decoder, err = videodec.NewH264()
		if err != nil {
			return nil, fmt.Errorf("RTSP H.264 decoding unavailable; select H.264 output or enable a decoder: %w", err)
		}
	}
	pixels, err := d.decoder.Decode(f.Data)
	if err != nil || pixels == nil {
		return nil, err
	}
	f.Data, f.Width, f.Height, f.Format = pixels.Data, pixels.Width, pixels.Height, capture.FormatRaw
	if decode || codec == "rawvideo" {
		return f, nil
	}
	if q == 0 {
		q = 80
	}
	f.Data, err = jpegcodec.EncodeBGRA(f.Data, f.Width, f.Height, q)
	f.Format = capture.FormatJPEG
	return f, err
}
func (d *Device) Close() error {
	d.life.Lock()
	defer d.life.Unlock()
	d.mu.Lock()
	s := d.s
	d.s = nil
	d.mu.Unlock()
	if s != nil {
		s.stop(nil)
		<-s.finished
	}
	d.decodeMu.Lock()
	defer d.decodeMu.Unlock()
	if d.decoder != nil {
		d.decoder.Close()
		d.decoder = nil
	}
	return nil
}
func (d *Device) SetDecodeImage(v bool)  { d.mu.Lock(); d.decode = v; d.mu.Unlock() }
func (d *Device) SetJPEGQuality(v int)   { d.mu.Lock(); d.quality = v; d.mu.Unlock() }
func (d *Device) SetVideoCodec(v string) { d.mu.Lock(); d.codec = v; d.mu.Unlock() }
func (*Device) SupportsVideoCodec(v string) bool {
	return v == "" || v == "mjpeg" || v == "rawvideo" || v == "h264"
}
func (*Device) PreserveCaptureMode() bool    { return true }
func (*Device) CaptureFPS() int              { return 0 }
func (*Device) SupportsGameStreamHEVC() bool { return false }
func (*Device) SupportsGameStreamAV1() bool  { return false }

type track struct {
	control string
	dep     h264RTP
}

func parseSDP(p []byte, base string) (track, string, error) {
	var t track
	var aggregate, control string
	var video, media bool
	var payload int = -1
	params := map[int]string{}
	for _, line := range strings.Split(strings.ReplaceAll(string(p), "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "m=") {
			fields := strings.Fields(line[2:])
			if video && payload >= 0 {
				break
			}
			media = true
			video = len(fields) >= 4 && fields[0] == "video"
			control = ""
			continue
		}
		if strings.HasPrefix(line, "a=control:") {
			if video {
				control = strings.TrimPrefix(line, "a=control:")
				if t.dep.pt >= 0 && payload >= 0 {
					t.control = control
				}
			} else if !media && aggregate == "" {
				aggregate = strings.TrimPrefix(line, "a=control:")
			}
		}
		if !video {
			continue
		}
		if strings.HasPrefix(line, "a=rtpmap:") {
			fields := strings.Fields(strings.TrimPrefix(line, "a=rtpmap:"))
			if len(fields) == 2 && strings.EqualFold(fields[1], "H264/90000") {
				n, e := strconv.Atoi(fields[0])
				if e == nil && n >= 0 && n < 128 {
					payload = n
					t.dep.pt = n
					t.control = control
				}
			}
		}
		if strings.HasPrefix(line, "a=fmtp:") {
			fields := strings.SplitN(strings.TrimPrefix(line, "a=fmtp:"), " ", 2)
			if len(fields) == 2 {
				n, e := strconv.Atoi(fields[0])
				if e == nil {
					params[n] = fields[1]
				}
			}
		}
	}
	if payload < 0 {
		return t, "", fmt.Errorf("RTSP source must offer H.264/90000 video")
	}
	for _, param := range strings.Split(params[payload], ";") {
		pair := strings.SplitN(strings.TrimSpace(param), "=", 2)
		if len(pair) != 2 {
			continue
		}
		if pair[0] == "packetization-mode" && pair[1] != "0" && pair[1] != "1" {
			return t, "", fmt.Errorf("RTSP H.264 interleaved packetization mode is unsupported")
		}
		if pair[0] == "sprop-parameter-sets" {
			for _, encoded := range strings.Split(pair[1], ",") {
				nal, e := base64.StdEncoding.DecodeString(encoded)
				if e != nil {
					return t, "", fmt.Errorf("invalid RTSP SDP parameter set")
				}
				t.dep.parameter(nal)
			}
		}
	}
	resolve := func(ref string) (string, error) { return controlURL(base, ref) }
	var err error
	t.control, err = resolve(t.control)
	if err != nil {
		return t, "", err
	}
	aggregate, err = resolve(aggregate)
	return t, aggregate, err
}

// h264RTP implements RFC 6184 single NAL, STAP-A and FU-A. A lost packet
// discards the entire access unit and waits for an IDR before resuming output.
type h264RTP struct {
	pt                          int
	seq                         uint16
	stamp, ssrc                 uint32
	started, fu, damaged, ready bool
	au, sps, pps                []byte
	w, h                        int
}

func (d *h264RTP) parameter(nal []byte) {
	if len(nal) == 0 {
		return
	}
	switch nal[0] & 31 {
	case 7:
		if info, e := videodec.ParseSPS(nal); e == nil {
			d.w, d.h = info.Width, info.Height
			d.sps = append([]byte(nil), nal...)
		}
	case 8:
		d.pps = append([]byte(nil), nal...)
	}
}
func (d *h264RTP) push(p []byte) ([]byte, int, int, error) {
	if len(p) < 12 || p[0]>>6 != 2 {
		return nil, 0, 0, fmt.Errorf("invalid RTSP RTP header")
	}
	if int(p[1]&127) != d.pt {
		return nil, 0, 0, nil
	}
	off := 12 + int(p[0]&15)*4
	if off > len(p) {
		return nil, 0, 0, io.ErrUnexpectedEOF
	}
	if p[0]&16 != 0 {
		if off+4 > len(p) {
			return nil, 0, 0, io.ErrUnexpectedEOF
		}
		off += 4 + int(binary.BigEndian.Uint16(p[off+2:]))*4
	}
	end := len(p)
	if p[0]&32 != 0 {
		pad := int(p[end-1])
		if pad < 1 || pad > end-off {
			return nil, 0, 0, fmt.Errorf("invalid RTP padding")
		}
		end -= pad
	}
	if off >= end {
		return nil, 0, 0, io.ErrUnexpectedEOF
	}
	seq, stamp, ssrc := binary.BigEndian.Uint16(p[2:]), binary.BigEndian.Uint32(p[4:]), binary.BigEndian.Uint32(p[8:])
	if d.started && (seq != d.seq+1 || ssrc != d.ssrc) {
		d.au = nil
		d.fu = false
		d.damaged = true
		d.ready = false
	}
	if d.started && stamp != d.stamp {
		if len(d.au) > 0 || d.fu {
			d.ready = false
		}
		d.au = nil
		d.fu = false
		d.damaged = false
	}
	d.started, d.seq, d.stamp, d.ssrc = true, seq, stamp, ssrc
	n := p[off:end]
	kind := n[0] & 31
	add := func(n []byte) { d.parameter(n); d.au = append(d.au, 0, 0, 0, 1); d.au = append(d.au, n...) }
	switch {
	case kind > 0 && kind < 24:
		if d.fu {
			d.damaged = true
		}
		add(n)
	case kind == 24:
		for q := n[1:]; len(q) > 0; {
			if len(q) < 2 {
				return nil, 0, 0, io.ErrUnexpectedEOF
			}
			size := int(binary.BigEndian.Uint16(q))
			q = q[2:]
			if size == 0 || size > len(q) {
				return nil, 0, 0, fmt.Errorf("invalid RTP STAP-A")
			}
			if q[0]&31 == 0 || q[0]&31 >= 24 {
				return nil, 0, 0, fmt.Errorf("invalid aggregated NAL")
			}
			add(q[:size])
			q = q[size:]
		}
	case kind == 28:
		if len(n) < 3 {
			return nil, 0, 0, io.ErrUnexpectedEOF
		}
		if n[1]&128 != 0 {
			if d.fu {
				d.damaged = true
			}
			d.fu = true
			d.au = append(d.au, 0, 0, 0, 1, (n[0]&0xe0)|(n[1]&31))
		} else if !d.fu {
			d.damaged = true
		}
		d.au = append(d.au, n[2:]...)
		if n[1]&64 != 0 {
			d.fu = false
		}
	default:
		return nil, 0, 0, fmt.Errorf("unsupported H.264 RTP packet type %d", kind)
	}
	if len(d.au) > 16<<20 {
		return nil, 0, 0, fmt.Errorf("RTSP access unit too large")
	}
	if p[1]&128 == 0 {
		return nil, 0, 0, nil
	}
	au := d.au
	d.au = nil
	if d.fu || d.damaged {
		d.fu = false
		d.damaged = false
		d.ready = false
		return nil, 0, 0, nil
	}
	idr := false
	for _, nal := range videodec.SplitNALs(au) {
		if len(nal) > 0 && nal[0]&31 == 5 {
			idr = true
		}
	}
	if idr && len(d.sps) > 0 && len(d.pps) > 0 {
		d.ready = true
		au = append(append(append([]byte{0, 0, 0, 1}, d.sps...), 0, 0, 0, 1), append(d.pps, au...)...)
	}
	if !d.ready || d.w == 0 || d.h == 0 {
		return nil, 0, 0, nil
	}
	return au, d.w, d.h, nil
}

func controlURL(base, ref string) (string, error) {
	u, e := url.Parse(base)
	if e != nil || u.Hostname() == "" || (u.Scheme != "rtsp" && u.Scheme != "rtsps") {
		return "", fmt.Errorf("invalid RTSP content base")
	}
	if ref == "" || ref == "*" {
		return u.String(), nil
	}
	r, e := url.Parse(ref)
	if e != nil || strings.ContainsAny(ref, "\r\n\x00") {
		return "", fmt.Errorf("invalid RTSP control URL")
	}
	resolved := u.ResolveReference(r)
	if resolved.Host != u.Host || resolved.Scheme != u.Scheme || resolved.User != nil {
		return "", fmt.Errorf("RTSP control URL changed origin")
	}
	return resolved.String(), nil
}
func (*Device) CaptureIdleStatus() (bool, string) { return true, "RTSP waits for source media" }
