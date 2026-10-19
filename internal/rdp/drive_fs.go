package rdp

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"golang.org/x/net/webdav"
)

// driveFS translates WebDAV operations into bounded MS-RDPEFS requests. No file
// content is staged on the server; the HTTP consumer controls transfer demand.
type driveFS struct{ c *client }
type driveInfo struct {
	name       string
	size       int64
	modified   time.Time
	attributes uint32
}

func (i driveInfo) Name() string       { return i.name }
func (i driveInfo) Size() int64        { return i.size }
func (i driveInfo) ModTime() time.Time { return i.modified }
func (i driveInfo) IsDir() bool        { return i.attributes&0x10 != 0 }
func (i driveInfo) Mode() fs.FileMode {
	if i.IsDir() {
		return fs.ModeDir | 0755
	}
	return 0644
}
func (i driveInfo) Sys() any { return nil }
func driveTime(n uint64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(int64(n/10000000)-11644473600, int64(n%10000000)*100)
}
func driveWide(s string, terminated bool) []byte {
	chars := utf16.Encode([]rune(s))
	if terminated {
		chars = append(chars, 0)
	}
	result := make([]byte, len(chars)*2)
	for i, ch := range chars {
		le.PutUint16(result[i*2:], ch)
	}
	return result
}
func driveText(p []byte) (string, error) {
	if len(p)%2 != 0 {
		return "", errProtocol
	}
	chars := make([]uint16, len(p)/2)
	for i := range chars {
		chars[i] = le.Uint16(p[i*2:])
		if chars[i] == 0 {
			return "", errProtocol
		}
	}
	for i := 0; i < len(chars); i++ {
		if chars[i] >= 0xd800 && chars[i] <= 0xdbff {
			if i+1 >= len(chars) || chars[i+1] < 0xdc00 || chars[i+1] > 0xdfff {
				return "", errProtocol
			}
			i++
		} else if chars[i] >= 0xdc00 && chars[i] <= 0xdfff {
			return "", errProtocol
		}
	}
	return string(utf16.Decode(chars)), nil
}
func (f *driveFS) writable() error {
	if f.c.s.cfg.DriveRedirection.Permissions&2 == 0 {
		return fs.ErrPermission
	}
	return nil
}
func (f *driveFS) resolve(name string) (*redirectedDrive, string, error) {
	if !validDrivePath(name) {
		return nil, "", fs.ErrPermission
	}
	parts := strings.SplitN(strings.Trim(name, "/"), "/", 2)
	if parts[0] == "" {
		return nil, "", nil
	}
	d := &f.c.drives
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, "", fs.ErrNotExist
	}
	for _, dev := range d.devices {
		if parts[0] == dev.name {
			relative := "\\"
			if len(parts) == 2 {
				relative += strings.ReplaceAll(strings.TrimSuffix(parts[1], "/"), "/", "\\")
			}
			return dev, relative, nil
		}
	}
	return nil, "", fs.ErrNotExist
}
func (f *driveFS) roots() []fs.FileInfo {
	d := &f.c.drives
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []fs.FileInfo
	for _, dev := range d.devices {
		out = append(out, driveInfo{name: dev.name, attributes: 0x10})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}
func (f *driveFS) checkParents(ctx context.Context, name string) error {
	if !validDrivePath(name) {
		return fs.ErrPermission
	}
	// Reject existing reparse-point ancestors as well as the final entry.
	// The client remains responsible for enforcing its exported filesystem root.
	parts := strings.Split(strings.Trim(name, "/"), "/")
	for i := 2; i < len(parts); i++ {
		parent, err := f.openDirect(ctx, strings.Join(parts[:i], "/"), 0x80, 1, 1)
		if err != nil {
			return err
		}
		if err = parent.Close(); err != nil {
			return err
		}
	}
	return nil
}
func (f *driveFS) open(ctx context.Context, name string, access, disposition, options uint32) (*driveFile, error) {
	if err := f.checkParents(ctx, name); err != nil {
		return nil, err
	}
	return f.openDirect(ctx, name, access, disposition, options)
}
func (f *driveFS) openDirect(ctx context.Context, name string, access, disposition, options uint32) (*driveFile, error) {
	dev, relative, err := f.resolve(name)
	if err != nil {
		return nil, err
	}
	file := &driveFile{fs: f, ctx: ctx, dev: dev, relative: relative, name: path.Base(strings.TrimSuffix(name, "/"))}
	if dev == nil {
		file.info = driveInfo{name: "/", attributes: 0x10}
		return file, nil
	}
	p := driveWide(relative, true)
	// OPEN_REPARSE_POINT lets us reject a link itself instead of following it.
	body := join(u32(access), make([]byte, 8), u32(0x80), u32(7), u32(disposition), u32(options|0x00200000), u32(uint32(len(p))), p)
	response, err := f.c.driveRPC(ctx, dev, 0, 0, 0, body)
	if err != nil {
		return nil, err
	}
	if len(response) < 4 {
		return nil, errProtocol
	}
	file.id = le.Uint32(response)
	if err = file.statRemote(); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}
func (f *driveFS) OpenFile(ctx context.Context, name string, flag int, perm fs.FileMode) (webdav.File, error) {
	write := flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0
	if write {
		if err := f.writable(); err != nil {
			return nil, err
		}
	}
	access := uint32(0x80000000)
	if write {
		access = 0x40000080
	}
	disposition := uint32(1)
	if flag&os.O_CREATE != 0 {
		disposition = 3
		if flag&os.O_EXCL != 0 {
			disposition = 2
		} else if flag&os.O_TRUNC != 0 {
			disposition = 5
		}
	} else if flag&os.O_TRUNC != 0 {
		disposition = 4
	}
	dev, relative, err := f.resolve(name)
	if err != nil {
		return nil, err
	}
	if write && (dev == nil || relative == "\\") {
		return nil, fs.ErrPermission
	}
	file, err := f.open(ctx, name, access, disposition, 0)
	if err != nil {
		return nil, err
	}
	file.canWrite = write
	if flag&os.O_APPEND != 0 {
		file.offset = file.info.size
	}
	return file, nil
}
func (f *driveFS) Stat(ctx context.Context, name string) (fs.FileInfo, error) {
	file, err := f.open(ctx, name, 0x80, 1, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return file.info, nil
}
func (f *driveFS) Mkdir(ctx context.Context, name string, perm fs.FileMode) error {
	if err := f.writable(); err != nil {
		return err
	}
	dev, relative, err := f.resolve(name)
	if err != nil {
		return err
	}
	if dev == nil || relative == "\\" {
		return fs.ErrPermission
	}
	file, err := f.open(ctx, name, 0x40000080, 2, 1)
	if err != nil {
		return err
	}
	return file.Close()
}
func (f *driveFS) Rename(ctx context.Context, oldName, newName string) error {
	if err := f.writable(); err != nil {
		return err
	}
	old, oldPath, err := f.resolve(oldName)
	if err != nil {
		return err
	}
	dev, relative, err := f.resolve(newName)
	if err != nil {
		return err
	}
	if old == nil || old != dev || relative == "\\" || oldPath == "\\" {
		return fs.ErrPermission
	}
	if err := f.checkParents(ctx, newName); err != nil {
		return err
	}
	file, err := f.open(ctx, oldName, 0x10080, 1, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	p := driveWide(relative, true)
	return file.setInfo(10, join([]byte{1, 0}, u32(uint32(len(p))), p))
}
func (f *driveFS) RemoveAll(ctx context.Context, name string) error {
	if err := f.writable(); err != nil {
		return err
	}
	return f.remove(ctx, name, 0)
}
func (f *driveFS) remove(ctx context.Context, name string, depth int) error {
	if depth > 128 {
		return fs.ErrInvalid
	}
	dev, relative, err := f.resolve(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if dev == nil || relative == "\\" {
		return fs.ErrPermission
	}
	file, err := f.open(ctx, name, 0x80010080, 1, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer file.Close()
	if file.info.IsDir() {
		entries, err := file.Readdir(-1)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := f.remove(ctx, path.Join(name, entry.Name()), depth+1); err != nil {
				return err
			}
		}
	}
	if err := file.setInfo(13, []byte{1}); err != nil {
		return err
	}
	return file.Close()
}

type driveFile struct {
	mu               sync.Mutex
	fs               *driveFS
	ctx              context.Context
	dev              *redirectedDrive
	id               uint32
	relative, name   string
	info             driveInfo
	offset           int64
	closed, canWrite bool
	entries          []fs.FileInfo
	enumerated       bool
}

func (f *driveFile) rpc(major, minor uint32, body []byte) ([]byte, error) {
	if f.closed {
		return nil, fs.ErrClosed
	}
	if f.dev == nil {
		return nil, fs.ErrInvalid
	}
	return f.fs.c.driveRPC(f.ctx, f.dev, f.id, major, minor, body)
}
func (f *driveFile) query(class uint32) ([]byte, error) {
	p, err := f.rpc(5, 0, join(u32(class), make([]byte, 28)))
	if err != nil {
		return nil, err
	}
	return driveBuffer(p)
}
func driveBuffer(p []byte) ([]byte, error) {
	if len(p) < 4 {
		return nil, errProtocol
	}
	n := int(le.Uint32(p))
	if n > len(p)-4 {
		return nil, errProtocol
	}
	return p[4 : 4+n], nil
}
func (f *driveFile) statRemote() error {
	basic, err := f.query(4)
	if err != nil {
		return err
	}
	if len(basic) < 36 {
		return errProtocol
	}
	standard, err := f.query(5)
	if err != nil {
		return err
	}
	if len(standard) < 22 {
		return errProtocol
	}
	size := le.Uint64(standard[8:])
	attrs := le.Uint32(basic[32:])
	if size > math.MaxInt64 {
		return fs.ErrInvalid
	}
	if attrs&0x400 != 0 {
		return fs.ErrPermission
	}
	if standard[21] != 0 {
		attrs |= 0x10
	}
	f.info = driveInfo{name: f.name, size: int64(size), modified: driveTime(le.Uint64(basic[16:])), attributes: attrs}
	return nil
}
func (f *driveFile) Stat() (fs.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, fs.ErrClosed
	}
	return f.info, nil
}
func (f *driveFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	if f.dev == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := f.fs.c.driveRPC(ctx, f.dev, f.id, 2, 0, make([]byte, 32))
	return err
}
func (f *driveFile) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fs.c.s.cfg.DriveRedirection.Permissions&1 == 0 {
		return 0, fs.ErrPermission
	}
	if f.info.IsDir() {
		return 0, fs.ErrInvalid
	}
	if len(p) == 0 {
		return 0, nil
	}
	n := min(len(p), driveChunk)
	response, err := f.rpc(3, 0, join(u32(uint32(n)), driveU64(uint64(f.offset)), make([]byte, 20)))
	if errors.Is(err, driveStatus(0xc0000011)) {
		return 0, io.EOF
	}
	if err != nil {
		return 0, err
	}
	data, err := driveBuffer(response)
	if err != nil {
		return 0, err
	}
	if len(data) > n || int64(len(data)) > math.MaxInt64-f.offset {
		return 0, errProtocol
	}
	copy(p, data)
	f.offset += int64(len(data))
	if len(data) == 0 {
		return 0, io.EOF
	}
	return len(data), nil
}
func (f *driveFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fs.writable(); err != nil {
		return 0, err
	}
	if !f.canWrite || f.info.IsDir() {
		return 0, fs.ErrPermission
	}
	written := 0
	for len(p) > 0 {
		n := min(len(p), driveChunk)
		if int64(n) > math.MaxInt64-f.offset {
			return written, fs.ErrInvalid
		}
		response, err := f.rpc(4, 0, join(u32(uint32(n)), driveU64(uint64(f.offset)), make([]byte, 20), p[:n]))
		if err != nil {
			return written, err
		}
		if len(response) < 4 {
			return written, errProtocol
		}
		count := int(le.Uint32(response))
		if count > n {
			return written, errProtocol
		}
		if count == 0 {
			return written, io.ErrShortWrite
		}
		p = p[count:]
		written += count
		f.offset += int64(count)
		f.info.modified = time.Now()
		f.info.size = max(f.info.size, f.offset)
	}
	return written, nil
}
func (f *driveFile) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, fs.ErrClosed
	}
	base := int64(0)
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = f.offset
	case io.SeekEnd:
		base = f.info.size
	default:
		return 0, fs.ErrInvalid
	}
	if offset < -base || offset > math.MaxInt64-base {
		return 0, fs.ErrInvalid
	}
	f.offset = base + offset
	return f.offset, nil
}
func (f *driveFile) setInfo(class uint32, data []byte) error {
	_, err := f.rpc(6, 0, join(u32(class), u32(uint32(len(data))), make([]byte, 24), data))
	return err
}
func (f *driveFile) Readdir(count int) ([]fs.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, fs.ErrClosed
	}
	if !f.info.IsDir() {
		return nil, fs.ErrInvalid
	}
	if !f.enumerated {
		if f.dev == nil {
			f.entries = f.fs.roots()
		} else {
			initial := true
			total := 0
			for {
				var pattern []byte
				first := byte(0)
				if initial {
					pattern = driveWide(strings.TrimSuffix(f.relative, "\\")+"\\*", true)
					first = 1
				}
				response, err := f.rpc(12, 1, join(u32(1), []byte{first}, u32(uint32(len(pattern))), make([]byte, 23), pattern))
				if errors.Is(err, driveStatus(0x80000006)) {
					break
				}
				if err != nil {
					return nil, err
				}
				data, err := driveBuffer(response)
				if err != nil {
					return nil, err
				}
				if len(data) == 0 {
					break
				}
				total += len(data)
				if total > 8<<20 {
					return nil, fs.ErrInvalid
				}
				entries, err := parseDriveDirectory(data)
				if err != nil {
					return nil, err
				}
				f.entries = append(f.entries, entries...)
				if len(f.entries) > 20000 {
					return nil, fs.ErrInvalid
				}
				initial = false
			}
		}
		f.enumerated = true
	}
	if len(f.entries) == 0 && count > 0 {
		return nil, io.EOF
	}
	n := len(f.entries)
	if count > 0 {
		n = min(n, count)
	}
	out := f.entries[:n]
	f.entries = f.entries[n:]
	return out, nil
}
func parseDriveDirectory(p []byte) ([]fs.FileInfo, error) {
	var out []fs.FileInfo
	for len(p) > 0 {
		if len(p) < 64 {
			return nil, errProtocol
		}
		next, n := int(le.Uint32(p)), int(le.Uint32(p[60:]))
		limit := len(p)
		if next != 0 {
			if next < 64 || next > len(p) {
				return nil, errProtocol
			}
			limit = next
		}
		if n > limit-64 {
			return nil, errProtocol
		}
		name, err := driveText(p[64 : 64+n])
		if err != nil {
			return nil, err
		}
		size := le.Uint64(p[40:])
		if size > math.MaxInt64 {
			return nil, errProtocol
		}
		attrs := le.Uint32(p[56:])
		if name != "." && name != ".." {
			if !validDriveName(name) {
				return nil, errProtocol
			}
			if attrs&0x400 == 0 {
				out = append(out, driveInfo{name: name, size: int64(size), modified: driveTime(le.Uint64(p[24:])), attributes: attrs})
			}
		}
		if next == 0 {
			break
		}
		p = p[next:]
	}
	return out, nil
}

func driveU64(n uint64) []byte { p := make([]byte, 8); le.PutUint64(p, n); return p }
