package rdp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"anykvm/internal/capture"
	"anykvm/internal/logging"
	"golang.org/x/text/unicode/norm"
)

const clipboardReceiveChunk = 256 << 10
const clipboardReceiveTimeout = 30 * time.Second

type clipboardFileReply struct {
	data []byte
	err  error
}

type clipboardFileRead struct {
	size  uint32
	reply chan clipboardFileReply
}

type remoteClipboardFiles struct {
	ctx        context.Context
	cancel     context.CancelFunc
	release    func()
	files      []capture.ClipboardFile
	generation uint64
	sizes      []int64 // Protected by clipboardState.mu.
}

// Called with clipboardState.mu held. Cancellation must never wait for a
// native callback, which may be waiting for responses from this protocol reader.
func (c *client) retireRemoteClipboardFiles() {
	if remote := c.clipboard.remoteFiles; remote != nil {
		c.clipboard.remoteFiles = nil
		remote.cancel()
		if remote.release != nil {
			remote.release()
		}
	}
}

func clipboardRemoteFileFormat(p []byte, long bool, ascii bool) (uint32, error) {
	var found uint32
	for len(p) != 0 {
		if len(p) < 4 {
			return 0, errProtocol
		}
		id := le.Uint32(p)
		p = p[4:]
		var name string
		if !long {
			if len(p) < 32 {
				return 0, errProtocol
			}
			if ascii {
				name = string(p[:32])
				name = strings.SplitN(name, "\x00", 2)[0]
			} else {
				var err error
				name, err = clipboardDecodeText(p[:32])
				if err != nil {
					return 0, err
				}
			}
			p = p[32:]
		} else {
			end := -1
			for i := 0; i+1 < len(p); i += 2 {
				if le.Uint16(p[i:]) == 0 {
					end = i + 2
					break
				}
			}
			if end < 0 {
				return 0, errProtocol
			}
			var err error
			name, err = clipboardDecodeText(p[:end])
			if err != nil {
				return 0, err
			}
			p = p[end:]
		}
		if name == clipFileGroupName {
			if id < 0xc000 || found != 0 {
				return 0, errProtocol
			}
			found = id
		}
	}
	return found, nil
}

func decodeClipboardFileList(p []byte) ([]capture.ClipboardFile, error) {
	if len(p) < 4 {
		return nil, errProtocol
	}
	count := uint64(le.Uint32(p))
	if count == 0 || count > maxClipboardFiles || uint64(len(p)) != 4+count*clipDescriptorSize {
		return nil, errProtocol
	}
	files := make([]capture.ClipboardFile, 0, count)
	seen, spellings, directories := make(map[string]bool), make(map[string]string), make(map[string]bool)
	for p = p[4:]; len(p) != 0; p = p[clipDescriptorSize:] {
		d := p[:clipDescriptorSize]
		flags, attrs := le.Uint32(d), le.Uint32(d[36:])
		if flags&4 == 0 || attrs&0x400 != 0 {
			return nil, errors.New("file list has missing attributes or reparse points")
		}
		name, err := clipboardDecodeText(d[72:])
		name = strings.ReplaceAll(name, "\\", "/")
		if err != nil || validateClipboardFileName(name) != nil {
			return nil, errors.New("invalid remote clipboard file name")
		}
		key := strings.ToLower(norm.NFC.String(name))
		if seen[key] || strings.ToLower(strings.Split(name, "/")[0]) == ".lock" {
			return nil, errors.New("duplicate or reserved remote clipboard name")
		}
		seen[key] = true
		isDir := attrs&0x10 != 0
		parts := strings.Split(name, "/")
		for j := range parts {
			prefix := strings.Join(parts[:j+1], "/")
			folded := strings.ToLower(norm.NFC.String(prefix))
			dir := j+1 < len(parts) || isDir
			if spelling, ok := spellings[folded]; ok && (spelling != prefix || directories[folded] != dir) {
				return nil, errors.New("ambiguous remote clipboard file tree")
			}
			spellings[folded], directories[folded] = prefix, dir
		}
		entry := capture.ClipboardFile{Name: name, Directory: isDir, Size: -1}
		if isDir {
			entry.Size = 0
		} else if flags&0x40 != 0 {
			size := uint64(le.Uint32(d[64:]))<<32 | uint64(le.Uint32(d[68:]))
			if size > uint64(maxClipboardFileBytes) {
				return nil, fmt.Errorf("remote clipboard file %s exceeds the single-file limit of %d bytes", name, maxClipboardFileBytes)
			}
			entry.Size = int64(size)
		}
		if flags&0x20 != 0 {
			stamp := le.Uint64(d[56:])
			seconds := stamp / 10000000
			if seconds <= 265046774399 {
				entry.Modified = time.Unix(int64(seconds)-11644473600, int64(stamp%10000000)*100)
			}
		}
		files = append(files, entry)
	}
	return files, nil
}

// Publishes only metadata and empty placeholders; the native consumer calls
// receive after a paste/read. The protocol reader remains free to deliver chunks.
func (c *client) publishRemoteClipboardFiles(p []byte) error {
	s := &c.clipboard
	files, err := decodeClipboardFileList(p)
	if err != nil {
		logging.Infof("rdp: clipboard file list: %v", err)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	remote := &remoteClipboardFiles{ctx: ctx, cancel: cancel, files: files, generation: s.generation}
	for _, file := range files {
		remote.sizes = append(remote.sizes, file.Size)
	}
	s.remoteFiles = remote
	revision, release, err := c.s.cfg.Clipboard.(capture.DeferredFileClipboard).WriteDeferredFiles(ctx,
		s.requestRevision, files, func(ctx context.Context, index int, file *os.File) error {
			return c.receiveClipboardFile(ctx, remote, index, file)
		})
	if err != nil {
		c.retireRemoteClipboardFiles()
		logging.Infof("rdp: clipboard file publication: %v", err)
		return nil
	}
	remote.release = release
	s.revision, s.haveRevision = revision, true
	return nil
}

func (c *client) receiveClipboardFile(ctx context.Context, remote *remoteClipboardFiles, index int, file *os.File) error {
	if index < 0 || index >= len(remote.files) || remote.files[index].Directory {
		return errors.New("invalid clipboard download index")
	}
	c.clipboard.mu.Lock()
	if c.clipboard.remoteFiles != remote || ctx.Err() != nil || remote.ctx.Err() != nil {
		c.clipboard.mu.Unlock()
		return errors.New("remote clipboard offer expired")
	}
	size := remote.sizes[index]
	c.clipboard.mu.Unlock()
	if size < 0 {
		data, err := c.requestClipboardFileChunk(ctx, remote, index, clipFileSize, 0, 8)
		if err != nil {
			return err
		}
		if len(data) != 8 {
			return errors.New("invalid remote clipboard file size")
		}
		length := le.Uint64(data)
		c.clipboard.mu.Lock()
		if length > uint64(maxClipboardFileBytes) {
			c.clipboard.mu.Unlock()
			return fmt.Errorf("remote clipboard file exceeds the single-file limit of %d bytes", maxClipboardFileBytes)
		}
		size = int64(length)
		remote.sizes[index] = size
		c.clipboard.mu.Unlock()
	}
	logging.Infof("rdp: clipboard download %s (%d bytes)", remote.files[index].Name, size)
	for offset := int64(0); offset < size; {
		requested := uint32(min(int64(clipboardReceiveChunk), size-offset))
		data, err := c.requestClipboardFileChunk(ctx, remote, index, clipFileRange, uint64(offset), requested)
		if err != nil {
			return err
		}
		if len(data) == 0 || len(data) > int(requested) {
			return errors.New("invalid remote clipboard file range")
		}
		n, err := file.WriteAt(data, offset)
		if err != nil {
			return err
		}
		if n != len(data) {
			return io.ErrShortWrite
		}
		offset += int64(n)
	}
	if err := file.Truncate(size); err != nil {
		return err
	}
	logging.Infof("rdp: clipboard download complete %s (%d bytes)", remote.files[index].Name, size)
	return nil
}

func (c *client) requestClipboardFileChunk(ctx context.Context, remote *remoteClipboardFiles,
	index int, kind uint32, offset uint64, size uint32) ([]byte, error) {
	s := &c.clipboard
	s.mu.Lock()
	if s.remoteFiles != remote || s.generation != remote.generation || ctx.Err() != nil || !c.clipboardReceiveFilesAllowed() {
		s.mu.Unlock()
		return nil, errors.New("remote clipboard offer expired")
	}
	select {
	case <-c.done:
		s.mu.Unlock()
		return nil, io.ErrClosedPipe
	default:
	}
	if len(s.fileReads) >= 64 || s.nextFileStream == ^uint32(0) {
		s.mu.Unlock()
		return nil, errors.New("clipboard file stream limit")
	}
	s.nextFileStream++
	id := s.nextFileStream
	request := &clipboardFileRead{size: size, reply: make(chan clipboardFileReply, 1)}
	if s.fileReads == nil {
		s.fileReads = make(map[uint32]*clipboardFileRead)
	}
	s.fileReads[id] = request
	err := c.sendClipboard(clipFileContentsRequest, 0, join(u32(id), u32(uint32(index)), u32(kind),
		u32(uint32(offset)), u32(uint32(offset>>32)), u32(size)))
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.fileReads, id); s.mu.Unlock() }()
	if err != nil {
		return nil, err
	}
	timer := time.NewTimer(clipboardReceiveTimeout)
	defer timer.Stop()
	select {
	case reply := <-request.reply:
		return reply.data, reply.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-remote.ctx.Done():
		return nil, remote.ctx.Err()
	case <-c.done:
		return nil, io.ErrClosedPipe
	case <-timer.C:
		return nil, errors.New("remote clipboard file read timed out")
	}
}

// Called with clipboardState.mu held. Stream IDs identify late replies across
// clipboard generations; unsolicited bytes never reach a local file.
func (c *client) handleClipboardFileResponse(flags uint16, p []byte) error {
	if len(p) < 4 || flags != clipOK && flags != clipFail {
		return fmt.Errorf("%w: clipboard file response", errProtocol)
	}
	request := c.clipboard.fileReads[le.Uint32(p)]
	if request == nil {
		return nil
	}
	delete(c.clipboard.fileReads, le.Uint32(p))
	reply := clipboardFileReply{}
	if flags == clipFail {
		reply.err = errors.New("remote clipboard file read failed")
	} else if len(p)-4 > int(request.size) {
		reply.err = errors.New("oversized remote clipboard file response")
	} else {
		reply.data = append([]byte(nil), p[4:]...)
	}
	request.reply <- reply
	return nil
}
