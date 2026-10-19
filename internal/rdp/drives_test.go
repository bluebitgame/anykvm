package rdp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// drivePeer implements the client side of RDPEFS against a private test tree.
// Requests cross the real MCS/static-channel framing before this peer sees them.
type drivePeer struct {
	t        *testing.T
	c        *client
	conn     *pointerRecordingConn
	root     string
	mu       sync.Mutex
	files    map[uint32]*peerDriveFile
	next     uint32
	offsets  []uint64
	requests []uint32
	failure  chan error
	stopped  chan struct{}
}
type peerDriveFile struct {
	file    *os.File
	name    string
	remove  bool
	entries []os.DirEntry
	listed  bool
}

func newDrivePeer(t *testing.T, permissions uint8) *drivePeer {
	t.Helper()
	return newDrivePeerWithMount(t, permissions, false, nil)
}

func newDrivePeerWithMount(t *testing.T, permissions uint8, autoMount bool, mounter driveMounter) *drivePeer {
	t.Helper()
	conn := &pointerRecordingConn{packets: make(chan []byte, 4096)}
	c := &client{s: &Server{cfg: Config{Username: "user", Password: "secret", DriveRedirection: DriveRedirectionOptions{Permissions: permissions, AutoMount: autoMount}}}, conn: conn, done: make(chan struct{}), data: clientData{drives: 1006}, logonSessionID: 7}
	c.drives.mounter = mounter
	p := &drivePeer{t: t, c: c, conn: conn, root: t.TempDir(), files: make(map[uint32]*peerDriveFile), failure: make(chan error, 1), stopped: make(chan struct{})}
	if err := os.WriteFile(filepath.Join(p.root, "你好.txt"), []byte("hello client"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.startDrives(); err != nil {
		t.Fatal(err)
	}
	go p.loop()
	t.Cleanup(func() {
		close(c.done)
		c.closeDrives()
		<-p.stopped
		c.s.wg.Wait()
		select {
		case err := <-p.failure:
			t.Error(err)
		default:
		}
		for _, file := range p.files {
			file.file.Close()
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.drives.mu.Lock()
		ready := c.drives.url != "" && len(c.drives.devices) == 1
		c.drives.mu.Unlock()
		if ready {
			return p
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("drive negotiation timed out")
	return nil
}
func (p *drivePeer) incoming(kind int, body []byte) error {
	msg := drivePDU(kind, body)
	// Fragment even small PDUs to exercise the incoming assembler.
	for offset := 0; offset < len(msg); {
		end := min(len(msg), offset+509)
		flags := uint32(0)
		if offset == 0 {
			flags |= 1
		}
		if end == len(msg) {
			flags |= 2
		}
		if err := p.c.handleDriveVC(join(u32(uint32(len(msg))), u32(flags), msg[offset:end])); err != nil {
			return err
		}
		offset = end
	}
	return nil
}
func (p *drivePeer) loop() {
	defer close(p.stopped)
	var message []byte
	total := 0
	for {
		select {
		case <-p.c.done:
			return
		case packet := <-p.conn.packets:
			wire, fast, err := readPacket(bytes.NewReader(packet))
			if err != nil || fast || len(wire) < 9 || be.Uint16(wire[6:]) != 1006 {
				p.failure <- fmt.Errorf("invalid drive transport")
				return
			}
			n, wire, err := takePer(wire[9:])
			if err != nil || n != len(wire) || len(wire) < 8 {
				p.failure <- errProtocol
				return
			}
			flags := le.Uint32(wire[4:])
			if flags&1 != 0 {
				total = int(le.Uint32(wire))
				message = nil
			}
			message = append(message, wire[8:]...)
			if flags&2 == 0 {
				continue
			}
			if len(message) != total {
				p.failure <- errProtocol
				return
			}
			if err := p.process(message); err != nil {
				p.failure <- err
				return
			}
		}
	}
}
func (p *drivePeer) process(message []byte) error {
	if len(message) < 4 || le.Uint16(message) != driveCore {
		return errProtocol
	}
	kind := le.Uint16(message[2:])
	body := message[4:]
	switch kind {
	case driveAnnounce:
		if err := p.incoming(driveConfirm, body); err != nil {
			return err
		}
		return p.incoming(driveName, join(u32(1), u32(0), u32(10), driveWide("test", true)))
	case driveServerCaps:
		if len(body) != 56 || le.Uint16(body) != 2 || le.Uint16(body[6:]) != 44 {
			return errors.New("invalid server capability layout")
		}
	case driveConfirm:
		general := join(u16(1), u16(44), u32(2), make([]byte, 20), u32(7), make([]byte, 12))
		if err := p.incoming(driveClientCaps, join(u16(2), u16(0), general, u16(4), u16(8), u32(2))); err != nil {
			return err
		}
		return p.incoming(driveDevices, join(u32(1), u32(8), u32(42), []byte{'C', 0, 0, 0, 0, 0, 0, 0}, u32(0)))
	case driveLoggedOn, driveReply:
	case driveRequest:
		if len(body) < 20 {
			return errProtocol
		}
		device, file, id, major, minor := le.Uint32(body), le.Uint32(body[4:]), le.Uint32(body[8:]), le.Uint32(body[12:]), le.Uint32(body[16:])
		p.mu.Lock()
		p.requests = append(p.requests, major)
		out, status, err := p.io(file, major, minor, body[20:])
		p.mu.Unlock()
		if err != nil {
			return err
		}
		return p.incoming(driveCompletion, join(u32(device), u32(id), u32(status), out))
	default:
		return fmt.Errorf("unexpected drive PDU %x", kind)
	}
	return nil
}
func (p *drivePeer) io(id, major, minor uint32, body []byte) ([]byte, uint32, error) {
	file := p.files[id]
	fail := func(err error) ([]byte, uint32, error) {
		status := uint32(0xc0000001)
		if errors.Is(err, fs.ErrNotExist) {
			status = 0xc0000034
		}
		if errors.Is(err, fs.ErrExist) {
			status = 0xc0000035
		}
		if errors.Is(err, fs.ErrPermission) {
			status = 0xc0000022
		}
		return nil, status, nil
	}
	if major == 0 {
		if len(body) < 32 || int(le.Uint32(body[28:])) != len(body)-32 {
			return nil, 0, errProtocol
		}
		text, err := driveText(body[32 : len(body)-2])
		if err != nil {
			return nil, 0, err
		}
		name := filepath.Join(p.root, strings.ReplaceAll(strings.TrimPrefix(text, "\\"), "\\", "/"))
		disposition, options := le.Uint32(body[20:]), le.Uint32(body[24:])
		flag := os.O_RDONLY
		if le.Uint32(body)&0x40000000 != 0 {
			flag = os.O_RDWR
		}
		switch disposition {
		case 1:
		case 2:
			flag |= os.O_CREATE | os.O_EXCL
		case 3:
			flag |= os.O_CREATE
		case 4:
			flag |= os.O_TRUNC
		case 5:
			flag |= os.O_CREATE | os.O_TRUNC
		default:
			return nil, 0, errProtocol
		}
		if options&1 != 0 && disposition == 2 {
			if err := os.Mkdir(name, 0700); err != nil {
				return fail(err)
			}
			flag = os.O_RDONLY
		}
		if info, err := os.Stat(name); err == nil && info.IsDir() {
			flag = os.O_RDONLY
		}
		opened, err := os.OpenFile(name, flag, 0600)
		if err != nil {
			return fail(err)
		}
		p.next++
		p.files[p.next] = &peerDriveFile{file: opened, name: name}
		return join(u32(p.next), []byte{1}), 0, nil
	}
	if file == nil {
		return nil, 0, fmt.Errorf("unknown file %d", id)
	}
	switch major {
	case 2:
		if len(body) != 32 {
			return nil, 0, errProtocol
		}
		err := file.file.Close()
		delete(p.files, id)
		if err == nil && file.remove {
			err = os.Remove(file.name)
		}
		if err != nil {
			return fail(err)
		}
		return make([]byte, 5), 0, nil
	case 5:
		if len(body) != 32 || le.Uint32(body[4:]) != 0 {
			return nil, 0, errProtocol
		}
		info, err := os.Lstat(file.name)
		if err != nil {
			return fail(err)
		}
		var data []byte
		switch le.Uint32(body) {
		case 4:
			data = make([]byte, 36)
			attrs := uint32(0x80)
			if info.IsDir() {
				attrs = 0x10
			}
			if info.Mode()&os.ModeSymlink != 0 {
				attrs |= 0x400
			}
			le.PutUint32(data[32:], attrs)
		case 5:
			data = make([]byte, 22)
			le.PutUint64(data[8:], uint64(info.Size()))
			if info.IsDir() {
				data[21] = 1
			}
		default:
			return nil, 0, errProtocol
		}
		return join(u32(uint32(len(data))), data), 0, nil
	case 3:
		if len(body) != 32 {
			return nil, 0, errProtocol
		}
		n, off := le.Uint32(body), le.Uint64(body[4:])
		p.offsets = append(p.offsets, off)
		if n > driveChunk {
			return nil, 0, errors.New("unbounded read")
		}
		data := make([]byte, n)
		count, err := file.file.ReadAt(data, int64(off))
		if err != nil && err != io.EOF {
			return fail(err)
		}
		if count == 0 {
			return nil, 0xc0000011, nil
		}
		return join(u32(uint32(count)), data[:count]), 0, nil
	case 4:
		if len(body) < 32 || int(le.Uint32(body)) != len(body)-32 || len(body)-32 > driveChunk {
			return nil, 0, errProtocol
		}
		off := le.Uint64(body[4:])
		p.offsets = append(p.offsets, off)
		n, err := file.file.WriteAt(body[32:], int64(off))
		if err != nil {
			return fail(err)
		}
		return join(u32(uint32(n)), []byte{0}), 0, nil
	case 12:
		if minor != 1 || len(body) < 32 || int(le.Uint32(body[5:])) != len(body)-32 {
			return nil, 0, errProtocol
		}
		if body[4] == 1 {
			var err error
			file.entries, err = os.ReadDir(file.name)
			if err != nil {
				return fail(err)
			}
			file.listed = true
		}
		if !file.listed {
			return nil, 0, errProtocol
		}
		if len(file.entries) == 0 {
			return []byte{0}, 0x80000006, nil
		}
		entry := file.entries[0]
		file.entries = file.entries[1:]
		info, err := entry.Info()
		if err != nil {
			return fail(err)
		}
		name := driveWide(entry.Name(), false)
		data := make([]byte, 64)
		le.PutUint64(data[40:], uint64(info.Size()))
		attrs := uint32(0x80)
		if info.IsDir() {
			attrs = 0x10
		}
		if info.Mode()&os.ModeSymlink != 0 {
			attrs |= 0x400
		}
		le.PutUint32(data[56:], attrs)
		le.PutUint32(data[60:], uint32(len(name)))
		data = append(data, name...)
		return join(u32(uint32(len(data))), data), 0, nil
	case 6:
		if len(body) < 32 || int(le.Uint32(body[4:])) != len(body)-32 {
			return nil, 0, errProtocol
		}
		data := body[32:]
		switch le.Uint32(body) {
		case 13:
			if len(data) != 1 {
				return nil, 0, errProtocol
			}
			file.remove = data[0] != 0
		case 10:
			if len(data) < 6 || int(le.Uint32(data[2:])) != len(data)-6 {
				return nil, 0, errProtocol
			}
			name, err := driveText(data[6 : len(data)-2])
			if err != nil {
				return nil, 0, err
			}
			target := filepath.Join(p.root, strings.ReplaceAll(strings.TrimPrefix(name, "\\"), "\\", "/"))
			if err = os.Rename(file.name, target); err != nil {
				return fail(err)
			}
			file.name = target
		default:
			return nil, 0, errProtocol
		}
		return u32(uint32(len(data))), 0, nil
	}
	return nil, 0, fmt.Errorf("unimplemented major %d", major)
}
func (p *drivePeer) request(t *testing.T, method, name string, body []byte, headers map[string]string) (int, []byte) {
	t.Helper()
	p.c.drives.mu.Lock()
	url := p.c.drives.url
	p.c.drives.mu.Unlock()
	request, err := http.NewRequest(method, url+name, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}
func TestDriveWebDAVReadWrite(t *testing.T) {
	p := newDrivePeer(t, 3)
	status, data := p.request(t, "PROPFIND", "", nil, map[string]string{"Depth": "1"})
	if status != 207 || !bytes.Contains(data, []byte("c/")) {
		t.Fatalf("root: %d %s", status, data)
	}
	status, data = p.request(t, "PROPFIND", "c/", nil, map[string]string{"Depth": "1"})
	if status != 207 || !bytes.Contains(data, []byte("%E4%BD%A0%E5%A5%BD.txt")) {
		t.Fatalf("directory: %d %s", status, data)
	}
	status, data = p.request(t, "GET", "c/你好.txt", nil, map[string]string{"Range": "bytes=6-11"})
	if status != 206 || string(data) != "client" {
		t.Fatalf("range: %d %s", status, data)
	}
	payload := bytes.Repeat([]byte("stream"), 100000)
	status, data = p.request(t, "PUT", "c/upload.bin", payload, nil)
	if status != 201 {
		t.Fatalf("put: %d %s", status, data)
	}
	actual, err := os.ReadFile(filepath.Join(p.root, "upload.bin"))
	if err != nil || !bytes.Equal(actual, payload) {
		t.Fatal("streamed upload differs", err)
	}
	status, data = p.request(t, "GET", "c/upload.bin", nil, nil)
	if status != 200 || !bytes.Equal(data, payload) {
		t.Fatal("streamed download differs", status)
	}
	status, data = p.request(t, "MKCOL", "c/folder", nil, nil)
	if status != 201 {
		t.Fatalf("mkdir: %d %s", status, data)
	}
	p.c.drives.mu.Lock()
	url := p.c.drives.url
	p.c.drives.mu.Unlock()
	status, data = p.request(t, "MOVE", "c/upload.bin", nil, map[string]string{"Destination": url + "c/folder/moved.bin"})
	if status != 201 {
		t.Fatalf("move: %d %s", status, data)
	}
	status, data = p.request(t, "COPY", "c/你好.txt", nil, map[string]string{"Destination": url + "c/copied.txt"})
	if status != 201 {
		t.Fatalf("copy: %d %s", status, data)
	}
	status, data = p.request(t, "DELETE", "c/folder", nil, nil)
	if status != 204 {
		t.Fatalf("delete: %d %s", status, data)
	}
	if _, err := os.Stat(filepath.Join(p.root, "folder")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("directory not removed", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.files) != 0 {
		t.Fatalf("remote handles leaked: %d", len(p.files))
	}
}
func TestDrivePermissions(t *testing.T) {
	for _, permissions := range []uint8{1, 2} {
		t.Run(fmt.Sprint(permissions), func(t *testing.T) {
			p := newDrivePeer(t, permissions)
			status, _ := p.request(t, "PROPFIND", "c/", nil, map[string]string{"Depth": "1"})
			if status != 207 {
				t.Fatal(status)
			}
			status, _ = p.request(t, "GET", "c/你好.txt", nil, nil)
			want := 200
			if permissions == 2 {
				want = 403
			}
			if status != want {
				t.Fatalf("get: %d want %d", status, want)
			}
			status, _ = p.request(t, "PUT", "c/put.txt", []byte("new"), nil)
			want = 201
			if permissions == 1 {
				want = 403
			}
			if status != want {
				t.Fatalf("put: %d want %d", status, want)
			}
			fsys := &driveFS{c: p.c}
			if permissions == 1 {
				if _, err := fsys.OpenFile(context.Background(), "/c/oops", os.O_CREATE|os.O_WRONLY, 0600); !errors.Is(err, fs.ErrPermission) {
					t.Fatal("filesystem write bypass", err)
				}
			} else {
				file, err := fsys.OpenFile(context.Background(), "/c/你好.txt", os.O_RDONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				if _, err = file.Read(make([]byte, 1)); !errors.Is(err, fs.ErrPermission) {
					t.Fatal("filesystem read bypass", err)
				}
			}
		})
	}
}
func TestDriveLargeOffsets(t *testing.T) {
	p := newDrivePeer(t, 3)
	sparse, err := os.Create(filepath.Join(p.root, "large.bin"))
	if err != nil {
		t.Fatal(err)
	}
	offset := int64(5<<30) + 17
	if _, err = sparse.WriteAt([]byte("tail"), offset); err != nil {
		t.Fatal(err)
	}
	sparse.Close()
	status, data := p.request(t, "GET", "c/large.bin", nil, map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", offset, offset+3)})
	if status != 206 || string(data) != "tail" {
		t.Fatalf("large range: %d %s", status, data)
	}
	fsys := &driveFS{c: p.c}
	file, err := fsys.OpenFile(context.Background(), "/c/large.bin", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Seek(offset+4, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte("!")); err != nil {
		t.Fatal(err)
	}
	file.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	found := false
	for _, n := range p.offsets {
		if n > 1<<32 {
			found = true
		}
	}
	if !found {
		t.Fatal("large offset truncated")
	}
}
func TestDriveLocalPathsAndDisconnect(t *testing.T) {
	p := newDrivePeer(t, 3)
	p.c.drives.mu.Lock()
	url := p.c.drives.url
	p.c.drives.mu.Unlock()
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode == 401 || response.Header.Get("WWW-Authenticate") != "" {
		t.Fatal("loopback endpoint requires authentication")
	}
	for _, name := range []string{"c/../secret", "c/%5csecret", "c/file:stream"} {
		status, _ := p.request(t, "GET", name, nil, nil)
		if status != 400 {
			t.Fatalf("unsafe path %s: %d", name, status)
		}
	}
	fsys := &driveFS{c: p.c}
	for _, name := range []string{"/c/../x", "/c/x\\y", "/c/a:b"} {
		if _, err := fsys.Stat(context.Background(), name); !errors.Is(err, fs.ErrPermission) {
			t.Fatal("unsafe backend path", name, err)
		}
	}
	if err := os.Symlink(filepath.Join(p.root, "你好.txt"), filepath.Join(p.root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.Stat(context.Background(), "/c/link"); !errors.Is(err, fs.ErrPermission) {
		t.Fatal("reparse link accepted", err)
	}
	p.c.closeDrives()
	client := http.Client{Timeout: time.Second}
	if response, err := client.Get(url); err == nil {
		response.Body.Close()
		t.Fatal("WebDAV survived disconnect")
	}
}
func TestDriveValidation(t *testing.T) {
	if err := (DriveRedirectionOptions{Permissions: 4}).Validate(); err == nil {
		t.Fatal("invalid permissions accepted")
	}
	for _, permissions := range []uint8{0} {
		conn := &pointerRecordingConn{packets: make(chan []byte, 8)}
		c := &client{s: &Server{cfg: Config{DriveRedirection: DriveRedirectionOptions{Permissions: permissions}}}, conn: conn, data: clientData{drives: 1006}}
		if err := c.startDrives(); err != nil {
			t.Fatal(err)
		}
		if len(conn.packets) != 0 {
			t.Fatal("disabled drive sent data")
		}
	}
}

func driveRPCTestClient() (*client, *pointerRecordingConn, *redirectedDrive) {
	conn := &pointerRecordingConn{packets: make(chan []byte, 32)}
	dev := &redirectedDrive{id: 42, name: "c"}
	c := &client{s: &Server{}, conn: conn, done: make(chan struct{}), data: clientData{drives: 1006}}
	c.drives.started = true
	c.drives.devices = map[uint32]*redirectedDrive{42: dev}
	c.drives.pending = make(map[uint32]*driveCall)
	c.drives.cleanup = make(chan driveCleanup, 128)
	return c, conn, dev
}
func driveNextRequest(t *testing.T, conn *pointerRecordingConn) []byte {
	t.Helper()
	select {
	case packet := <-conn.packets:
		wire, _, err := readPacket(bytes.NewReader(packet))
		if err != nil {
			t.Fatal(err)
		}
		_, wire, err = takePer(wire[9:])
		if err != nil || len(wire) < 32 {
			t.Fatal("invalid request", err)
		}
		return wire[8:]
	case <-time.After(time.Second):
		t.Fatal("no drive request")
		return nil
	}
}
func TestDriveRPCCancellationAndLateCreate(t *testing.T) {
	c, conn, dev := driveRPCTestClient()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := c.driveRPC(ctx, dev, 0, 0, 0, make([]byte, 32)); result <- err }()
	request := driveNextRequest(t, conn)
	id := le.Uint32(request[12:])
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := c.handleDrivePDU(drivePDU(driveCompletion, join(u32(42), u32(id), u32(0), u32(99), []byte{1}))); err != nil {
		t.Fatal(err)
	}
	select {
	case job := <-c.drives.cleanup:
		if job.file != 99 || job.device != dev {
			t.Fatal("incorrect late-create cleanup")
		}
	default:
		t.Fatal("late-create handle leaked")
	}
	c.drives.mu.Lock()
	defer c.drives.mu.Unlock()
	if len(c.drives.pending) != 0 {
		t.Fatal("completed call retained")
	}
}
func TestDriveRPCDeviceRemovalAndMismatch(t *testing.T) {
	c, conn, dev := driveRPCTestClient()
	result := make(chan error, 1)
	go func() { _, err := c.driveRPC(context.Background(), dev, 1, 3, 0, nil); result <- err }()
	request := driveNextRequest(t, conn)
	id := le.Uint32(request[12:])
	if err := c.handleDrivePDU(drivePDU(driveCompletion, join(u32(43), u32(id), u32(0), u32(0)))); !errors.Is(err, errProtocol) {
		t.Fatal("wrong device completion accepted", err)
	}
	if err := c.handleDrivePDU(drivePDU(driveRemove, join(u32(1), u32(42)))); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	// Reusing an ID must not revive an old file handle.
	c.drives.mu.Lock()
	c.drives.devices[42] = &redirectedDrive{id: 42, name: "c"}
	c.drives.mu.Unlock()
	if _, err := c.driveRPC(context.Background(), dev, 1, 4, 0, nil); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("stale device accepted", err)
	}
	if len(conn.packets) != 0 {
		t.Fatal("stale operation reached new device")
	}
}
func TestDriveRPCDisconnectAndBounds(t *testing.T) {
	c, conn, dev := driveRPCTestClient()
	result := make(chan error, 1)
	go func() { _, err := c.driveRPC(context.Background(), dev, 1, 3, 0, nil); result <- err }()
	driveNextRequest(t, conn)
	c.closeDrives()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("pending call survived disconnect")
		}
	case <-time.After(time.Second):
		t.Fatal("disconnect blocked RPC")
	}
	c, _, dev = driveRPCTestClient()
	c.drives.next = ^uint32(0)
	if _, err := c.driveRPC(context.Background(), dev, 1, 3, 0, nil); err == nil {
		t.Fatal("completion ID wrapped")
	}
	c.drives.next = 0
	for i := 0; i < 128; i++ {
		c.drives.pending[uint32(i)] = &driveCall{}
	}
	if _, err := c.driveRPC(context.Background(), dev, 1, 3, 0, nil); err == nil {
		t.Fatal("unbounded pending calls")
	}
}
func TestDriveChannelNegotiation(t *testing.T) {
	core := make([]byte, 142)
	le.PutUint16(core[4:], 1920)
	le.PutUint16(core[6:], 1080)
	le.PutUint16(core[140:], 0x100)
	for _, names := range [][]string{{"rdpdr", "cliprdr", "drdynvc"}, {"rdpdr", "rdpdr", "drdynvc"}} {
		channels := u32(uint32(len(names)))
		for _, name := range names {
			field := make([]byte, 8)
			copy(field, name)
			channels = join(channels, field, u32(0))
		}
		blocks := join(capability(0xc001, core), capability(0xc003, channels))
		gcc := join([]byte("Duca"), perLength(len(blocks)), blocks)
		value := join(berValue([]byte{4}, nil), berValue([]byte{4}, nil), berValue([]byte{1}, nil), berValue([]byte{0x30}, nil), berValue([]byte{0x30}, nil), berValue([]byte{0x30}, nil), berValue([]byte{4}, gcc))
		data, err := parseConnect(join([]byte{2, 0xf0, 0x80}, berValue([]byte{0x7f, 0x65}, value)))
		if names[1] == "rdpdr" {
			if err == nil {
				t.Fatal("duplicate drive channel accepted")
			}
		} else if err != nil || data.drives != 1004 || data.clipboard != 1005 || data.dvc != 1006 {
			t.Fatalf("channels: %+v %v", data, err)
		}
	}
}
func TestDriveMalformedInput(t *testing.T) {
	for _, packet := range [][]byte{nil, make([]byte, 7), join(u32(driveMaxMessage+1), u32(3)), join(u32(4), u32(2), make([]byte, 4)), join(u32(4), u32(0x103), make([]byte, 4)), join(u32(4), u32(3), make([]byte, 3)), join(u32(4), u32(3), make([]byte, 5))} {
		c, _, _ := driveRPCTestClient()
		if err := c.handleDriveVC(packet); err == nil {
			t.Fatalf("malformed fragment accepted: %x", packet)
		}
	}
	for _, message := range [][]byte{nil, drivePDU(driveDevices, u32(100)), drivePDU(driveRemove, join(u32(2), u32(1))), drivePDU(driveCompletion, make([]byte, 11)), drivePDU(driveName, make([]byte, 12)), drivePDU(driveClientCaps, make([]byte, 4))} {
		c, _, _ := driveRPCTestClient()
		if err := c.handleDrivePDU(message); err == nil {
			t.Fatalf("malformed PDU accepted: %x", message)
		}
	}
	name := driveWide("../bad", false)
	entry := make([]byte, 64)
	le.PutUint32(entry[60:], uint32(len(name)))
	entry = append(entry, name...)
	if _, err := parseDriveDirectory(entry); err == nil {
		t.Fatal("directory traversal name accepted")
	}
	le.PutUint32(entry[60:], 0xfffffffe)
	if _, err := parseDriveDirectory(entry); err == nil {
		t.Fatal("directory name length overflow")
	}
	if _, err := driveText([]byte{0, 0xd8}); err == nil {
		t.Fatal("unpaired surrogate accepted")
	}
}

func TestDriveMovePreflightAndAncestors(t *testing.T) {
	p := newDrivePeer(t, 3)
	p.c.drives.mu.Lock()
	url := p.c.drives.url
	p.c.drives.devices[43] = &redirectedDrive{id: 43, name: "D"}
	p.c.drives.mu.Unlock()
	for _, tc := range []struct{ src, dst string }{
		{"c/你好.txt", "D/你好.txt"}, // Cross-drive overwrite must fail before deletion.
		{"c/", "c/你好.txt"},
		{"c/你好.txt", "c/你好.TXT"},
	} {
		status, _ := p.request(t, "MOVE", tc.src, nil, map[string]string{"Destination": url + tc.dst, "Overwrite": "T"})
		if status != 403 {
			t.Fatal("unsafe MOVE accepted", tc, status)
		}
		if data, err := os.ReadFile(filepath.Join(p.root, "你好.txt")); err != nil || string(data) != "hello client" {
			t.Fatal("preflight modified destination", err)
		}
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(p.root, "linkdir")); err != nil {
		t.Fatal(err)
	}
	fsys := &driveFS{c: p.c}
	if _, err := fsys.Stat(context.Background(), "/c/linkdir/secret"); !errors.Is(err, fs.ErrPermission) {
		t.Fatal("symlink ancestor followed", err)
	}
	if err := fsys.Rename(context.Background(), "/c/你好.txt", "/c/linkdir/moved"); !errors.Is(err, fs.ErrPermission) {
		t.Fatal("rename through symlink ancestor", err)
	}
	for _, name := range []string{"/c//file", "/c/file.", "/c/file ", "/c/*"} {
		if _, err := fsys.Stat(context.Background(), name); !errors.Is(err, fs.ErrPermission) {
			t.Fatal("ambiguous path accepted", name, err)
		}
	}
	// Local WebDAV access does not depend on RDP credentials.
	p.c.s.UpdateRuntime(RuntimeConfig{Username: "other", Password: "changed"})
	status, _ := p.request(t, "GET", "c/你好.txt", nil, nil)
	if status != 200 {
		t.Fatal("local access depends on RDP credentials", status)
	}
}

func TestDriveStableRootHandshake(t *testing.T) {
	p := newDrivePeer(t, 3)
	p.c.drives.mu.Lock()
	endpoint := p.c.drives.url
	p.c.drives.mu.Unlock()
	if !strings.HasSuffix(endpoint, "/") || strings.Count(endpoint, "/") != 3 {
		t.Fatalf("Finder endpoint must be the server root: %s", endpoint)
	}
	status, _ := p.request(t, "OPTIONS", "", nil, nil)
	if status != 200 {
		t.Fatalf("authenticated root OPTIONS: %d", status)
	}
	status, data := p.request(t, "PROPFIND", "", nil, map[string]string{"Depth": "0"})
	if status != 207 || !bytes.Contains(data, []byte("<D:href>/</D:href>")) || bytes.Contains(data, []byte("/s/")) {
		t.Fatalf("root PROPFIND: %d %s", status, data)
	}
}

func TestDriveSessionAddressReconnect(t *testing.T) {
	first := newDrivePeer(t, 3)
	first.c.drives.mu.Lock()
	endpoint := first.c.drives.url
	first.c.drives.mu.Unlock()
	fsys := &driveFS{c: first.c}
	stale, err := fsys.OpenFile(context.Background(), "/c/你好.txt", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	first.c.closeDrives()
	second := newDrivePeer(t, 3)
	second.c.drives.mu.Lock()
	next := second.c.drives.url
	second.c.drives.mu.Unlock()
	if !strings.HasPrefix(next, "http://127.0.0.1:") {
		t.Fatalf("expected loopback endpoint: %s", next)
	}
	client := http.Client{Timeout: time.Second}
	if response, err := client.Get(endpoint); err == nil {
		response.Body.Close()
		t.Fatal("old session endpoint survived reconnect")
	}
	if err := os.WriteFile(filepath.Join(second.root, "你好.txt"), []byte("next client"), 0600); err != nil {
		t.Fatal(err)
	}
	status, data := second.request(t, "GET", "c/你好.txt", nil, nil)
	if status != 200 || string(data) != "next client" {
		t.Fatalf("reconnected root: %d %s", status, data)
	}
	if _, err := stale.Read(make([]byte, 1)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("old handle survived reconnect", err)
	}
	_ = stale.Close()
}

func TestDriveRejectsBrowserCrossSite(t *testing.T) {
	p := newDrivePeer(t, 3)
	p.c.drives.mu.Lock()
	endpoint := p.c.drives.url
	p.c.drives.mu.Unlock()
	for _, tc := range []struct {
		host, origin, site string
		want               int
	}{
		{"", "", "", 200},
		{"", strings.TrimSuffix(endpoint, "/"), "same-origin", 200},
		{"evil.example", "", "", 403},
		{"", "https://evil.example", "", 403},
		{"", "null", "", 403},
		{"", "", "cross-site", 403},
	} {
		req, err := http.NewRequest("OPTIONS", endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		if tc.host != "" {
			req.Host = tc.host
		}
		req.Header.Set("Origin", tc.origin)
		req.Header.Set("Sec-Fetch-Site", tc.site)
		response, err := (&http.Client{Timeout: time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != tc.want {
			t.Fatalf("host=%q origin=%q site=%q: %d", tc.host, tc.origin, tc.site, response.StatusCode)
		}
		if response.Header.Get("WWW-Authenticate") != "" {
			t.Fatal("local endpoint challenges for credentials")
		}
	}
}

func TestDriveDisplayNames(t *testing.T) {
	for _, tc := range []struct {
		name, preferred string
		data            []byte
		want            string
	}{
		{"DOS drive colon", "C:", nil, "c"},
		{"lowercase drive", "d:", nil, "d"},
		{"DOS drive root", "E:\\", nil, "e"},
		{"legacy folder", "Shared", nil, "Shared"},
		{"full name overrides DOS", "C:", driveWide("D:", true), "d"},
		{"full drive root", "", driveWide("F:\\", true), "f"},
		{"Unicode folder", "Shared", driveWide("共享文件夹😀", true), "共享文件夹😀"},
		{"full folder name", "Downloa", driveWide("Downloads", true), "Downloads"},
		{"single letter folder", "c", driveWide("c", true), "c"},
		{"uppercase single letter folder", "C", driveWide("C", true), "C"},
		{"legacy bare drive", "C", nil, "c"},
		{"full bare drive", "C:", driveWide("C", true), "c"},
		{"zero padded full drive", "C:", join(driveWide("D:", true), make([]byte, 4)), "d"},
		{"embedded NUL full name", "C:", driveWide("D\x00extra", true), "drive-9"},
		{"first NUL terminates DOS name", "G:\x00dirty", nil, "g"},
		{"missing name", "", nil, "drive-9"},
		{"invalid path", "../", nil, "drive-9"},
		{"embedded colon", "foo:bar", nil, "drive-9"},
		{"invalid Unicode name", "C:", []byte{0, 0xd8, 0, 0}, "drive-9"},
		{"unterminated full name", "C:", driveWide("D:", false), "drive-9"},
		{"odd Unicode length", "C:", []byte{'D', 0, 0}, "drive-9"},
		{"invalid full path", "C:", driveWide("../outside", true), "drive-9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := driveDisplayName([]byte(tc.preferred), tc.data, 9); got != tc.want {
				t.Fatalf("name=%q want %q", got, tc.want)
			}
		})
	}
}
func TestDriveAnnounceUsesClientNames(t *testing.T) {
	c, _, _ := driveRPCTestClient()
	c.drives.capable = true
	c.drives.devices = make(map[uint32]*redirectedDrive)
	for _, tc := range []struct {
		id              uint32
		dos, full, want string
	}{
		{1, "C:", "", "c"}, {2, "D:", "", "d"}, {3, "Shared", "共享文件夹", "共享文件夹"}, {4, "C:", "", "c-4-0"},
	} {
		dos := make([]byte, 8)
		copy(dos, tc.dos)
		var full []byte
		if tc.full != "" {
			full = driveWide(tc.full, true)
		}
		message := drivePDU(driveDevices, join(u32(1), u32(8), u32(tc.id), dos, u32(uint32(len(full))), full))
		if err := c.handleDrivePDU(message); err != nil {
			t.Fatal(err)
		}
		if got := c.drives.devices[tc.id].name; got != tc.want {
			t.Fatalf("device %d name %q want %q", tc.id, got, tc.want)
		}
	}
	fsys := &driveFS{c: c}
	dev, relative, err := fsys.resolve("/c/file.txt")
	if err != nil || dev.id != 1 || relative != "\\file.txt" {
		t.Fatalf("letter path resolved incorrectly: %v %q %v", dev, relative, err)
	}
}
