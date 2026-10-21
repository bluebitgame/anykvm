package rdp

// MS-RDPEFS drive redirection. The protocol reader only assembles messages and
// delivers completions; filesystem operations must run outside that reader.
import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/net/webdav"

	"anykvm/internal/logging"
)

// DriveRedirectionOptions exposes client-selected drives through loopback WebDAV.
// Permissions are from the server's perspective: 1=read, 2=write, 3=both.
// Directory metadata remains readable with either permission.
type DriveRedirectionOptions struct {
	Permissions uint8 `yaml:"permissions"`
	AutoMount   bool  `yaml:"auto_mount"` // macOS/Linux; defaults to true.
}

func (o DriveRedirectionOptions) Validate() error {
	if o.Permissions > 3 {
		return errors.New("rdp.drive.permissions must be 0..3")
	}
	return nil
}

const (
	driveCore       = 0x4472
	driveAnnounce   = 0x496e
	driveConfirm    = 0x4343
	driveName       = 0x434e
	driveServerCaps = 0x5350
	driveClientCaps = 0x4350
	driveLoggedOn   = 0x554c
	driveDevices    = 0x4441
	driveRemove     = 0x444d
	driveReply      = 0x6472
	driveRequest    = 0x4952
	driveCompletion = 0x4943
	driveChunk      = 256 << 10
	driveMaxMessage = 1 << 20
	driveTimeout    = 45 * time.Second
)

type redirectedDrive struct {
	id   uint32
	name string
}
type driveResult struct {
	status uint32
	body   []byte
	err    error
}
type driveCall struct {
	device    *redirectedDrive
	major     uint32
	reply     chan driveResult
	abandoned bool
}
type driveCleanup struct {
	device *redirectedDrive
	file   uint32
}
type driveState struct {
	mu              sync.Mutex
	started, closed bool
	// Only the protocol reader accesses handshake and fragment fields.
	confirmed, named, capable bool
	clientID                  uint32
	minor                     uint16
	buf                       []byte
	total                     int
	devices                   map[uint32]*redirectedDrive
	pending                   map[uint32]*driveCall
	next                      uint32
	server                    *http.Server
	clientName                string
	url                       string
	cleanup                   chan driveCleanup
	mountCancel               context.CancelFunc
	mounter                   driveMounter // Optional per-session test override.
}

func drivePDU(kind int, body []byte) []byte { return join(u16(driveCore), u16(kind), body) }
func (c *client) sendDrive(kind int, body []byte) error {
	return c.sendStaticVC(c.data.drives, drivePDU(kind, body))
}

func (c *client) startDrives() error {
	o := c.s.cfg.DriveRedirection
	if c.data.drives == 0 || o.Permissions == 0 {
		return nil
	}
	d := &c.drives
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started || d.closed {
		return nil
	}
	d.started = true
	d.devices = make(map[uint32]*redirectedDrive)
	d.pending = make(map[uint32]*driveCall)
	d.cleanup = make(chan driveCleanup, 128)
	d.minor = 12
	d.clientID = c.logonSessionID
	// The accept loop is started only after drive capability negotiation.
	c.s.wg.Add(1)
	go func() {
		defer c.s.wg.Done()
		for {
			select {
			case <-c.done:
				return
			case job := <-d.cleanup:
				_, _ = c.driveRPC(context.Background(), job.device, job.file, 2, 0, make([]byte, 32))
			}
		}
	}()
	return c.sendDrive(driveAnnounce, join(u16(1), u16(12), u32(d.clientID)))
}

func (c *client) closeDrives() {
	d := &c.drives
	d.mu.Lock()
	d.closed = true
	srv := d.server
	cancel := d.mountCancel
	d.devices = nil
	for id, call := range d.pending {
		call.reply <- driveResult{err: net.ErrClosed}
		delete(d.pending, id)
	}
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if srv != nil {
		_ = srv.Close()
	}
}

func (c *client) startDriveWebDAV() error {
	d := &c.drives
	// The OS allocates a private loopback endpoint for this RDP session.
	// The mount root is always /; native mounting receives the chosen URL.
	const prefix = "/"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	handler := &webdav.Handler{Prefix: prefix, FileSystem: &driveFS{c: c}, LockSystem: webdav.NewMemLS()}
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 90 * time.Second, WriteTimeout: 90 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32 << 10}
	// Limit simultaneous slow filesystem clients independently of the RDP RPC cap.
	slots := make(chan struct{}, 16)
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Loopback is intentionally password-free. Reject browser requests from
		// other sites and DNS rebinding hosts before they can access client files.
		origin := r.Header.Get("Origin")
		if r.Host != ln.Addr().String() || (origin != "" && origin != "http://"+ln.Addr().String()) || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			http.Error(w, "Local access only", http.StatusForbidden)
			return
		}
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		if !validDrivePath(strings.TrimPrefix(r.URL.Path, prefix)) {
			http.Error(w, "Invalid path", 400)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			http.Error(w, "Busy", 503)
			return
		}
		// Refresh idle deadlines as bytes move, allowing arbitrarily large transfers.
		ctl := http.NewResponseController(w)
		r.Body = &driveHTTPBody{ReadCloser: r.Body, ctl: ctl}
		w = &driveHTTPWriter{ResponseWriter: w, ctl: ctl}
		switch r.Method {
		case "GET", "HEAD", "POST":
			if c.s.cfg.DriveRedirection.Permissions&1 == 0 {
				http.Error(w, "Read permission denied", 403)
				return
			}
		case "OPTIONS", "PROPFIND", "UNLOCK":
		case "COPY":
			if c.s.cfg.DriveRedirection.Permissions != 3 {
				http.Error(w, "Read and write permissions required", 403)
				return
			}
		default:
			if c.s.cfg.DriveRedirection.Permissions&2 == 0 {
				http.Error(w, "Write permission denied", 403)
				return
			}
		}
		if r.Method == "MOVE" || r.Method == "COPY" {
			if err := c.validateDriveDestination(r, prefix); err != nil {
				http.Error(w, "Invalid or unsupported destination", 403)
				return
			}
		}
		handler.ServeHTTP(w, r)
	})
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		ln.Close()
		return net.ErrClosed
	}
	d.server = srv
	d.url = "http://" + ln.Addr().String() + prefix
	url := d.url
	d.mu.Unlock()
	c.s.wg.Add(1)
	go func() {
		defer c.s.wg.Done()
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logging.Infof("rdp: drive WebDAV: %v", err)
		}
	}()
	logging.Infof("rdp: client drives WebDAV: %s (local access, permissions=%d)", url, c.s.cfg.DriveRedirection.Permissions)
	return nil
}

func (c *client) handleDriveVC(p []byte) error {
	d := &c.drives
	d.mu.Lock()
	enabled := d.started && !d.closed
	d.mu.Unlock()
	if !enabled {
		return nil
	}
	if len(p) < 8 {
		return errProtocol
	}
	total, flags := int(le.Uint32(p)), le.Uint32(p[4:])
	if total < 4 || total > driveMaxMessage || flags & ^uint32(0xf3) != 0 {
		return errProtocol
	}
	if flags&1 != 0 {
		if d.total != 0 {
			return errProtocol
		}
		d.total = total
		d.buf = nil
	}
	if d.total != total || len(p)-8 > total-len(d.buf) {
		return errProtocol
	}
	d.buf = append(d.buf, p[8:]...)
	if flags&2 == 0 {
		return nil
	}
	if len(d.buf) != total {
		return errProtocol
	}
	message := d.buf
	d.buf = nil
	d.total = 0
	return c.handleDrivePDU(message)
}

func (c *client) handleDrivePDU(p []byte) error {
	if len(p) < 4 || le.Uint16(p) != driveCore {
		return errProtocol
	}
	kind := le.Uint16(p[2:])
	p = p[4:]
	d := &c.drives
	switch kind {
	case driveConfirm:
		if d.confirmed || len(p) != 8 || le.Uint16(p) != 1 || le.Uint16(p[2:]) < 10 {
			return errProtocol
		}
		d.confirmed = true
		d.minor = min(le.Uint16(p[2:]), uint16(12))
		d.clientID = le.Uint32(p[4:])
	case driveName:
		if !d.confirmed || d.named || len(p) < 12 {
			return errProtocol
		}
		n := int(le.Uint32(p[8:]))
		if n < 1 || n > 512 || n != len(p)-12 || p[len(p)-1] != 0 {
			return errProtocol
		}
		if le.Uint32(p)&1 != 0 && (n%2 != 0 || n < 2 || p[len(p)-2] != 0) {
			return errProtocol
		}
		name := string(p[12 : len(p)-1])
		if le.Uint32(p)&1 != 0 {
			// Ignore an unusable name without rejecting the drive handshake.
			name, _ = driveText(p[12 : len(p)-2])
		}
		d.mu.Lock()
		d.clientName = name
		d.mu.Unlock()
		d.named = true
		general := join(u16(1), u16(44), u32(2), u32(0), u32(0), u16(1), u16(int(d.minor)), u32(0xffff), u32(0), u32(7), u32(0), u32(0), u32(0))
		if err := c.sendDrive(driveServerCaps, join(u16(2), u16(0), general, u16(4), u16(8), u32(2))); err != nil {
			return err
		}
		return c.sendDrive(driveConfirm, join(u16(1), u16(int(d.minor)), u32(d.clientID)))
	case driveClientCaps:
		if !d.named || d.capable || len(p) < 4 {
			return errProtocol
		}
		count := int(le.Uint16(p))
		p = p[4:]
		general, drive, logged := false, false, false
		if count > 32 {
			return errProtocol
		}
		for i := 0; i < count; i++ {
			if len(p) < 8 {
				return errProtocol
			}
			n := int(le.Uint16(p[2:]))
			if n < 8 || n > len(p) {
				return errProtocol
			}
			switch le.Uint16(p) {
			case 1:
				if n < 40 {
					return errProtocol
				}
				general = true
				logged = le.Uint32(p[28:])&4 != 0
			case 4:
				drive = true
			}
			p = p[n:]
		}
		if len(p) != 0 || !general {
			return errProtocol
		}
		d.capable = true
		if !drive {
			return nil
		}
		if logged {
			if err := c.sendDrive(driveLoggedOn, nil); err != nil {
				return err
			}
		}
		if err := c.startDriveWebDAV(); err != nil {
			logging.Infof("rdp: cannot expose client drives: %v", err)
		}
	case driveDevices:
		if !d.capable || len(p) < 4 {
			return errProtocol
		}
		count := int(le.Uint32(p))
		p = p[4:]
		if count > 64 {
			return errProtocol
		}
		for i := 0; i < count; i++ {
			if len(p) < 20 {
				return errProtocol
			}
			typ, id, n := le.Uint32(p), le.Uint32(p[4:]), int(le.Uint32(p[16:]))
			if n > len(p)-20 {
				return errProtocol
			}
			name := driveDisplayName(p[8:16], p[20:20+n], id)
			p = p[20+n:]
			status := uint32(0xc00000bb)
			if typ == 8 {
				if !validDriveName(name) {
					name = fmt.Sprintf("drive-%d", id)
				}
				d.mu.Lock()
				if d.devices[id] != nil || len(d.devices) >= 64 {
					d.mu.Unlock()
					return errProtocol
				}
				base := name
				for suffix := 0; ; suffix++ {
					collision := false
					for _, other := range d.devices {
						if strings.EqualFold(name, other.name) {
							collision = true
							break
						}
					}
					if !collision {
						break
					}
					name = fmt.Sprintf("%s-%d-%d", base, id, suffix)
				}
				d.devices[id] = &redirectedDrive{id: id, name: name}
				d.mu.Unlock()
				status = 0
				logging.Infof("rdp: client drive available: %s", name)
			}
			if err := c.sendDrive(driveReply, join(u32(id), u32(status))); err != nil {
				return err
			}
		}
		if len(p) != 0 {
			return errProtocol
		}
		c.startDriveMount()
	case driveRemove:
		if len(p) < 4 {
			return errProtocol
		}
		n := int(le.Uint32(p))
		p = p[4:]
		if n > 64 || len(p) != n*4 {
			return errProtocol
		}
		d.mu.Lock()
		for i := 0; i < n; i++ {
			id := le.Uint32(p[i*4:])
			delete(d.devices, id)
			for key, call := range d.pending {
				if call.device.id == id {
					call.reply <- driveResult{err: fs.ErrNotExist}
					delete(d.pending, key)
				}
			}
		}
		d.mu.Unlock()
	case driveCompletion:
		if len(p) < 12 {
			return errProtocol
		}
		device, id, status := le.Uint32(p), le.Uint32(p[4:]), le.Uint32(p[8:])
		d.mu.Lock()
		call := d.pending[id]
		if call == nil {
			d.mu.Unlock()
			return nil
		}
		if call.device.id != device {
			d.mu.Unlock()
			return errProtocol
		}
		delete(d.pending, id)
		if call.abandoned && call.major == 0 && status == 0 && len(p) >= 16 {
			select {
			case d.cleanup <- driveCleanup{call.device, le.Uint32(p[12:])}:
			default:
				d.mu.Unlock()
				return errProtocol
			}
		} else {
			call.reply <- driveResult{status: status, body: append([]byte(nil), p[12:]...)}
		}
		d.mu.Unlock()
	default:
		return errProtocol
	}
	return nil
}

func (c *client) driveRPC(ctx context.Context, dev *redirectedDrive, file, major, minor uint32, body []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, driveTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d := &c.drives
	d.mu.Lock()
	if d.closed || d.devices[dev.id] != dev {
		d.mu.Unlock()
		return nil, fs.ErrNotExist
	}
	if len(d.pending) >= 128 {
		d.mu.Unlock()
		return nil, errors.New("too many pending drive operations")
	}
	// Never reuse a completion ID, even after cancellation.
	if d.next == ^uint32(0) {
		d.mu.Unlock()
		return nil, errors.New("drive completion IDs exhausted; reconnect")
	}
	d.next++
	id := d.next
	call := &driveCall{device: dev, major: major, reply: make(chan driveResult, 1)}
	d.pending[id] = call
	// Serialize request admission with device removal/reuse, but never wait for
	// a completion under this mutex. Transport writes have a five-second deadline.
	err := c.sendDrive(driveRequest, join(u32(dev.id), u32(file), u32(id), u32(major), u32(minor), body))
	if err != nil {
		delete(d.pending, id)
	}
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case result := <-call.reply:
		if result.err != nil {
			return nil, result.err
		}
		if result.status != 0 {
			return nil, driveError(result.status, dev.name)
		}
		return result.body, nil
	case <-ctx.Done():
		d.mu.Lock()
		if d.pending[id] == call {
			call.abandoned = true
			d.mu.Unlock()
			return nil, ctx.Err()
		}
		// A completion raced cancellation; consume it so an opened handle is not leaked.
		d.mu.Unlock()
		result := <-call.reply
		if major == 0 && result.status == 0 && result.err == nil && len(result.body) >= 4 {
			select {
			case d.cleanup <- driveCleanup{dev, le.Uint32(result.body)}:
			case <-c.done:
			}
		}
		return nil, ctx.Err()
	case <-c.done:
		return nil, net.ErrClosed
	}
}

type driveStatus uint32

func (s driveStatus) Error() string { return fmt.Sprintf("RDP filesystem status 0x%08x", uint32(s)) }
func (s driveStatus) Is(target error) bool {
	switch uint32(s) {
	case 0xc000000f, 0xc0000034, 0xc000003a:
		return target == fs.ErrNotExist
	case 0xc0000022, 0xc00000a2:
		return target == fs.ErrPermission
	case 0xc0000035:
		return target == fs.ErrExist
	}
	return false
}
func validDriveName(name string) bool {
	return name != "" && name != "." && name != ".." && utf8.ValidString(name) &&
		!strings.HasSuffix(name, ".") && !strings.HasSuffix(name, " ") &&
		!strings.ContainsAny(name, "/\\:\x00*?") && strings.IndexFunc(name, func(r rune) bool { return r < 32 || r == 127 }) < 0
}
func validDrivePath(name string) bool {
	if len(name) > 32760 || !utf8.ValidString(name) || strings.ContainsAny(name, "\\:\x00") {
		return false
	}
	trimmed := strings.Trim(name, "/")
	if trimmed == "" {
		return true
	}
	parts := strings.Split(trimmed, "/")
	if len(parts) > 129 {
		return false
	}
	for _, part := range parts {
		if !validDriveName(part) {
			return false
		}
	}
	return true
}

// x/net/webdav uses os.IsNotExist/IsPermission, which only unwrap PathError
// and do not consult custom error Is methods.
func driveError(status uint32, name string) error {
	s := driveStatus(status)
	for _, base := range []error{fs.ErrNotExist, fs.ErrPermission, fs.ErrExist} {
		if s.Is(base) {
			return &fs.PathError{Op: s.Error(), Path: name, Err: base}
		}
	}
	return s
}

type driveHTTPBody struct {
	io.ReadCloser
	ctl *http.ResponseController
}

func (b *driveHTTPBody) Read(p []byte) (int, error) {
	_ = b.ctl.SetReadDeadline(time.Now().Add(90 * time.Second))
	return b.ReadCloser.Read(p)
}

type driveHTTPWriter struct {
	http.ResponseWriter
	ctl *http.ResponseController
}

func (w *driveHTTPWriter) Write(p []byte) (int, error) {
	_ = w.ctl.SetWriteDeadline(time.Now().Add(90 * time.Second))
	return w.ResponseWriter.Write(p)
}
func (w *driveHTTPWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Preflight before x/net/webdav can remove an overwrite destination. In
// particular, cross-device MOVE and case-only aliases must never delete it first.
func (c *client) validateDriveDestination(r *http.Request, prefix string) error {
	destination, err := url.Parse(r.Header.Get("Destination"))
	if err != nil || !strings.HasPrefix(destination.Path, prefix) {
		return fs.ErrInvalid
	}
	f := &driveFS{c: c}
	src, srcPath, err := f.resolve(strings.TrimPrefix(r.URL.Path, prefix))
	if err != nil {
		return err
	}
	dst, dstPath, err := f.resolve(strings.TrimPrefix(destination.Path, prefix))
	if err != nil {
		return err
	}
	if dst == nil || dstPath == "\\" {
		return fs.ErrPermission
	}
	if r.Method == "MOVE" && (src == nil || src != dst || srcPath == "\\") {
		return fs.ErrPermission
	}
	if src == dst {
		source, target := strings.ToLower(srcPath), strings.ToLower(dstPath)
		if source == target || strings.HasPrefix(target, strings.TrimSuffix(source, "\\")+"\\") {
			return fs.ErrInvalid
		}
	}
	return nil
}

// MS-RDPEFS 2.2.3.1: nonempty DeviceData carries the full UTF-16LE
// name and supersedes PreferredDosName. A DOS trailing colon is a valid drive
// designator, but not a safe component in the virtual WebDAV filesystem.
// Drive letters use WSL-style lowercase names; folder names retain their case.
func driveDisplayName(preferred, data []byte, id uint32) string {
	fallback := fmt.Sprintf("drive-%d", id)
	var name string
	fullName := len(data) != 0
	if i := strings.IndexByte(string(preferred), 0); i >= 0 {
		preferred = preferred[:i]
	}
	preferredName := string(preferred)
	if fullName {
		if len(data) < 2 || len(data) > 2048 || len(data)%2 != 0 || le.Uint16(data[len(data)-2:]) != 0 {
			return fallback
		}
		// Some clients pad the terminated name with additional zero units.
		for len(data) > 0 && le.Uint16(data[len(data)-2:]) == 0 {
			data = data[:len(data)-2]
		}
		var err error
		name, err = driveText(data)
		if err != nil {
			return fallback
		}
	} else {
		name = preferredName
	}
	if len(name) >= 1 && ((name[0] >= 'A' && name[0] <= 'Z') || (name[0] >= 'a' && name[0] <= 'z')) {
		switch {
		case len(name) == 1 && (!fullName || strings.EqualFold(preferredName, name+":")):
			name = strings.ToLower(name)
		case len(name) == 2 && name[1] == ':', len(name) == 3 && name[1] == ':' && (name[2] == '\\' || name[2] == '/'):
			name = strings.ToLower(name[:1])
		}
	}
	if len(name) > 1024 || !validDriveName(name) {
		return fallback
	}
	return name
}
