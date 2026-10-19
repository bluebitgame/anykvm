package rdp

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"anykvm/internal/capture"
	"anykvm/internal/videodec"
)

// SourceOptions configures an upstream, separately from the RDP listener.
type SourceOptions struct {
	Username           string        `yaml:"username,omitempty"`
	Password           string        `yaml:"password,omitempty"`
	PasswordEnv        string        `yaml:"password_env,omitempty"`
	Domain             string        `yaml:"domain,omitempty"`
	Timeout            time.Duration `yaml:"timeout,omitempty"`
	Security           string        `yaml:"security,omitempty"` // nla (default) or tls
	InsecureSkipVerify bool          `yaml:"insecure_skip_verify,omitempty"`
}

func IsSource(raw string) bool { return strings.HasPrefix(strings.ToLower(raw), "rdp:") }
func sourceAddress(raw string) (string, error) {
	u, e := url.Parse(raw)
	if len(raw) > 16384 || e != nil || u.Scheme != "rdp" || u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(raw, "\r\n\x00") {
		return "", fmt.Errorf("RDP source requires rdp://host[:port] with optional user:password credentials")
	}
	port := u.Port()
	if port == "" {
		port = "3389"
	}
	n, e := strconv.Atoi(port)
	if e != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("invalid RDP source port")
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}
func ValidateSource(raw string, o SourceOptions) error {
	if !IsSource(raw) {
		return nil
	}
	if _, e := sourceAddress(raw); e != nil {
		return e
	}
	if o.Security != "" && o.Security != "nla" && o.Security != "tls" {
		return fmt.Errorf("rdp.security must be nla or tls")
	}
	if o.Timeout < 0 || o.Timeout > 5*time.Minute {
		return fmt.Errorf("rdp.timeout must be 0..5m (0 uses 15s)")
	}
	if o.Password != "" && o.PasswordEnv != "" {
		return fmt.Errorf("use only one of rdp.password and password_env")
	}
	if len(utf16Bytes(o.Username))+len(utf16Bytes(o.Domain))+len(utf16Bytes(o.Password)) > 8192 {
		return fmt.Errorf("RDP source credentials too long")
	}
	return nil
}

type SourceClient struct {
	address  string
	opts     SourceOptions
	life, mu sync.Mutex
	s        *sourceSession
	epoch    uint64
}

func NewSource(raw string, o SourceOptions) (capture.Device, bool) {
	if !IsSource(raw) {
		return nil, false
	}
	if e := ValidateSource(raw, o); e != nil {
		return capture.Failed(e), true
	}
	u, _ := url.Parse(raw)
	if u.User != nil {
		if o.Username == "" {
			o.Username = u.User.Username()
		}
		if o.Password == "" && o.PasswordEnv == "" {
			o.Password, _ = u.User.Password()
		}
	}
	if o.Domain == "" {
		if domain, user, ok := strings.Cut(o.Username, "\\"); ok {
			o.Domain, o.Username = domain, user
		}
	}
	address, _ := sourceAddress(raw)
	return &SourceClient{address: address, opts: o}, true
}
func (c *SourceClient) current() *sourceSession { c.mu.Lock(); defer c.mu.Unlock(); return c.s }
func (c *SourceClient) Open() error             { return c.OpenWithFormat(0, 0, 0) }
func (c *SourceClient) OpenWithFormat(w, h, _ int) error {
	c.life.Lock()
	defer c.life.Unlock()
	if c.current() != nil {
		return fmt.Errorf("RDP source already open")
	}
	if w <= 0 || h <= 0 {
		w, h = 1920, 1080
	}
	if w > 8192 || h > 8192 || w*h > 16<<20 {
		return fmt.Errorf("RDP source desktop too large")
	}
	password := c.opts.Password
	if c.opts.PasswordEnv != "" {
		var ok bool
		password, ok = os.LookupEnv(c.opts.PasswordEnv)
		if !ok {
			return fmt.Errorf("rdp.password_env is not set")
		}
	}
	for _, value := range []string{c.opts.Username, c.opts.Domain, password} {
		if len(utf16Bytes(value)) > 4096 {
			return fmt.Errorf("RDP source credential exceeds 4096 UTF-16 bytes")
		}
	}
	decoder, err := videodec.NewH264()
	if err != nil {
		return fmt.Errorf("RDP source needs H.264 decoding for surface composition: %w", err)
	}
	timeout := c.opts.Timeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	conn, err := net.DialTimeout("tcp", c.address, timeout)
	if err != nil {
		decoder.Close()
		return err
	}
	c.mu.Lock()
	c.epoch++
	epoch := c.epoch
	c.mu.Unlock()
	s := &sourceSession{conn: conn, timeout: timeout, opts: c.opts, decoder: decoder, done: make(chan struct{}), finished: make(chan struct{}), ready: make(chan struct{}), frames: make(chan *capture.Frame, 2), audio: make(chan *capture.AudioFrame, 5), surfaces: make(map[uint16]*sourceSurface), epoch: epoch, width: w, height: h, vc: make(map[uint16]*sourceFragments)}
	conn.SetDeadline(time.Now().Add(timeout))
	if err = s.handshake(c.address, password, w, h); err != nil {
		conn.Close()
		decoder.Close()
		return fmt.Errorf("RDP source: %w", err)
	}
	c.mu.Lock()
	c.s = s
	c.mu.Unlock()
	go s.run()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.ready:
		return nil
	case <-s.done:
		err = s.failure()
	case <-timer.C:
		err = fmt.Errorf("RDP server did not negotiate H.264/AVC420 before timeout")
	}
	s.stop(err)
	<-s.finished
	c.mu.Lock()
	c.s = nil
	c.mu.Unlock()
	return err
}
func (c *SourceClient) Capture() (*capture.Frame, error) {
	s := c.current()
	if s == nil {
		return nil, capture.ErrStreamStopped
	}
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-s.done:
		return nil, s.failure()
	case f := <-s.frames:
		return f, nil
	case <-timer.C:
		return nil, nil
	}
}
func (c *SourceClient) Close() error {
	c.life.Lock()
	defer c.life.Unlock()
	s := c.current()
	if s == nil {
		return nil
	}
	_ = c.KeyUp()
	for button := 0; button < 3; button++ {
		_ = c.MouseButton(button, false)
	}
	s.stop(nil)
	<-s.finished
	c.mu.Lock()
	c.s = nil
	c.mu.Unlock()
	return nil
}
func (*SourceClient) SetDecodeImage(bool)       {}
func (*SourceClient) SetJPEGQuality(int)        {}
func (*SourceClient) PreserveCaptureMode() bool { return true }
func (c *SourceClient) CaptureIdleStatus() (bool, string) {
	s := c.current()
	if s == nil {
		return false, "RDP disconnected"
	}
	select {
	case <-s.done:
		return false, "RDP disconnected"
	default:
		return true, "RDP waits for desktop changes"
	}
}
func (*SourceClient) SupportsGameStreamHEVC() bool { return false }
func (*SourceClient) SupportsGameStreamAV1() bool  { return false }
