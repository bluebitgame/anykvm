package rdp

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"anykvm/internal/capture"
	"anykvm/internal/clipboard"
	"golang.org/x/text/unicode/norm"
)

const (
	clipFileContentsRequest  = 8
	clipFileContentsResponse = 9
	clipFileGroupDescriptor  = 0xc001 // Session-local registered format, identified by its name.
	clipFileGroupName        = "FileGroupDescriptorW"
	clipFileSize             = 1
	clipFileRange            = 2
	clipStreamFiles          = 4
	clipNoFilePaths          = 8
	clipDescriptorSize       = 592
	maxClipboardFiles        = 1024
	maxClipboardFileBytes    = int64(1<<31 - 1) // Windows-compatible limit per file, not per collection.
	maxClipboardFileChunk    = 4 << 20          // Allow normal Explorer reads, including a full 1 MiB request.
)

// A copy pins file handles, not file contents. Range requests read those handles
// only when the client asks. Replacing a path cannot redirect a published index
// to an unrelated file, and a changed size/mtime invalidates that file's offer.
type clipboardFile struct {
	name string
	info fs.FileInfo
	file *os.File // nil for directories
}

type clipboardFiles struct {
	entries []clipboardFile
	data    []byte
}

func (files *clipboardFiles) close() {
	if files == nil {
		return
	}
	for _, entry := range files.entries {
		if entry.file != nil {
			entry.file.Close()
		}
	}
}

func (c *client) clipboardFilesAllowed() bool {
	return c.clipboardSendFilesAllowed() || c.clipboardReceiveFilesAllowed()
}

func (c *client) clipboardSendFilesAllowed() bool {
	_, supported := c.s.cfg.Clipboard.(capture.FileClipboard)
	options := c.s.cfg.ClipboardOptions
	return !c.s.cfg.DisableClipboard && supported && options != nil && options.Files.Allows(clipboard.ServerToClient)
}

func (c *client) clipboardReceiveFilesAllowed() bool {
	_, supported := c.s.cfg.Clipboard.(capture.DeferredFileClipboard)
	options := c.s.cfg.ClipboardOptions
	return !c.s.cfg.DisableClipboard && supported && options != nil && options.Files.Allows(clipboard.ClientToServer)
}

func (c *client) cleanupClipboardFiles() {
	c.clipboard.mu.Lock()
	defer c.clipboard.mu.Unlock()
	c.clipboard.localFiles.close()
	c.clipboard.localFiles = nil
	c.retireRemoteClipboardFiles()
}

// Called with the clipboard mutex held. File contents never enter the pasteboard
// or a staging directory; each response is read directly from a selected file.
func (c *client) handleClipboardFileRequest(p []byte) error {
	if len(p) < 4 {
		return fmt.Errorf("%w: clipboard file request stream ID", errProtocol)
	}
	stream := p[:4]
	fail := func() error { return c.sendClipboard(clipFileContentsResponse, clipFail, stream) }
	s := &c.clipboard
	// clipDataId is only legal when locking was negotiated. This implementation
	// does not advertise locking, so 28-byte requests must fail as well.
	if len(p) != 24 || !c.clipboardSendFilesAllowed() || !s.peerFiles || !s.listAccepted || s.localFiles == nil {
		return fail()
	}
	_, _, revision, err := c.s.cfg.Clipboard.ReadText()
	c.clipboardBackendResult(err)
	if err != nil || revision != s.revision {
		return fail() // Do not serve a stale index after another host copy.
	}
	index := le.Uint32(p[4:])
	if uint64(index) >= uint64(len(s.localFiles.entries)) {
		return fail()
	}
	entry := &s.localFiles.entries[index]
	if entry.file == nil {
		return fail()
	}
	info, err := entry.file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxClipboardFileBytes || info.Size() != entry.info.Size() || !info.ModTime().Equal(entry.info.ModTime()) {
		return fail()
	}
	flags, requested := le.Uint32(p[8:]), le.Uint32(p[20:])
	offset := uint64(le.Uint32(p[12:])) | uint64(le.Uint32(p[16:]))<<32
	var data []byte
	switch flags {
	case clipFileSize:
		if offset != 0 || requested != 8 {
			return fail()
		}
		data = make([]byte, 8)
		le.PutUint64(data, uint64(info.Size()))
	case clipFileRange:
		if requested > maxClipboardFileChunk || offset > uint64(info.Size()) {
			return fail()
		}
		size := min(uint64(requested), uint64(info.Size())-offset)
		data = make([]byte, int(size))
		n, err := entry.file.ReadAt(data, int64(offset))
		if err != nil && err != io.EOF || n != len(data) {
			return fail()
		}
		// Detect writes during the read before returning bytes to the client.
		info, err = entry.file.Stat()
		if err != nil || info.Size() != entry.info.Size() || !info.ModTime().Equal(entry.info.ModTime()) {
			return fail()
		}
	default:
		return fail()
	}
	return c.sendClipboard(clipFileContentsResponse, clipOK, join(stream, data))
}

func snapshotClipboardFiles(paths []string) (_ *clipboardFiles, err error) {
	files := &clipboardFiles{}
	defer func() {
		if err != nil {
			files.close()
		}
	}()
	if len(paths) == 0 || len(paths) > maxClipboardFiles {
		return nil, fmt.Errorf("clipboard file count exceeds limit")
	}
	names := make(map[string]bool)
	for _, source := range paths {
		if !filepath.IsAbs(source) {
			return nil, fmt.Errorf("clipboard source must be absolute")
		}
		source = filepath.Clean(source)
		// Walk a rooted tree without following symlinks. OpenRoot additionally
		// prevents a concurrently replaced directory from escaping the selection.
		root, openErr := os.OpenRoot(filepath.Dir(source))
		if openErr != nil {
			return nil, openErr
		}
		base := filepath.Base(source)
		selected, statErr := root.Lstat(base)
		if statErr != nil || selected.Mode()&os.ModeSymlink != 0 {
			root.Close()
			return nil, fmt.Errorf("clipboard selection is unavailable or a symbolic link")
		}
		walkBase, prefix := base, ""
		if selected.IsDir() {
			tree, err := root.OpenRoot(base)
			root.Close()
			if err != nil {
				return nil, err
			}
			root, walkBase, prefix = tree, ".", base
			opened, err := root.Stat(".")
			if err != nil || !os.SameFile(selected, opened) {
				root.Close()
				return nil, fmt.Errorf("clipboard selection changed during copy")
			}
		}
		walkErr := fs.WalkDir(root.FS(), walkBase, func(name string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if len(files.entries) >= maxClipboardFiles {
				return fmt.Errorf("clipboard file count exceeds limit")
			}
			wireName := filepath.ToSlash(filepath.Join(prefix, name))
			if err := validateClipboardFileName(wireName); err != nil {
				return err
			}
			key := strings.ToLower(norm.NFC.String(wireName))
			if names[key] {
				return fmt.Errorf("duplicate clipboard file name: %s", wireName)
			}
			names[key] = true
			// DirEntry.Info from os.Root.FS can resolve the original pathname
			// on Go 1.24. Use a rooted lstat even if a directory was renamed.
			info, err := root.Lstat(name)
			if err != nil {
				return err
			}
			entry := clipboardFile{name: strings.ReplaceAll(wireName, "/", "\\"), info: info}
			if !info.IsDir() {
				if !info.Mode().IsRegular() || info.Size() < 0 {
					return fmt.Errorf("unsupported clipboard file type: %s", wireName)
				}
				if info.Size() > maxClipboardFileBytes {
					return fmt.Errorf("clipboard file %s exceeds the single-file limit of %d bytes", wireName, maxClipboardFileBytes)
				}
				file, err := openClipboardSource(root, name)
				if err != nil {
					return err
				}
				opened, err := file.Stat()
				if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() ||
					opened.Size() != info.Size() || !opened.ModTime().Equal(info.ModTime()) {
					file.Close()
					return fmt.Errorf("clipboard source changed during copy")
				}
				entry.file = file
			}
			files.entries = append(files.entries, entry)
			return nil
		})
		root.Close()
		if walkErr != nil {
			return nil, walkErr
		}
	}
	files.data = make([]byte, 4+len(files.entries)*clipDescriptorSize)
	le.PutUint32(files.data, uint32(len(files.entries)))
	for i, entry := range files.entries {
		p := files.data[4+i*clipDescriptorSize:][:clipDescriptorSize]
		le.PutUint32(p, 0x04|0x20|0x40|0x4000) // Attributes, write time, size, progress UI.
		attributes, size := uint32(0x80), uint64(entry.info.Size())
		if entry.info.IsDir() {
			attributes, size = 0x10, 0
		}
		le.PutUint32(p[36:], attributes)
		modified := entry.info.ModTime()
		if modified.Unix() >= -11644473600 && modified.Unix() < 1<<40 {
			le.PutUint64(p[56:], uint64(modified.Unix()+11644473600)*10000000+uint64(modified.Nanosecond()/100))
		} else {
			le.PutUint32(p, 0x04|0x40|0x4000)
		}
		le.PutUint32(p[64:], uint32(size>>32))
		le.PutUint32(p[68:], uint32(size))
		for j, unit := range utf16.Encode([]rune(entry.name)) {
			le.PutUint16(p[72+2*j:], unit)
		}
	}
	return files, nil
}

// Restrict names to safe, unambiguous Windows relative paths. The same rules
// reject ambiguous names on case-insensitive macOS volumes and Windows devices.
func validateClipboardFileName(name string) error {
	if !utf8.ValidString(name) || len(utf16.Encode([]rune(name))) >= 260 {
		return fmt.Errorf("clipboard file name exceeds Windows limit")
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return fmt.Errorf("invalid clipboard file name")
		}
		for _, r := range part {
			if r < 32 || strings.ContainsRune("\\:<>\"|?*", r) {
				return fmt.Errorf("invalid clipboard file name")
			}
		}
		stem := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" || stem == "CONIN$" || stem == "CONOUT$" ||
			len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9' {
			return fmt.Errorf("reserved Windows clipboard file name")
		}
	}
	return nil
}

func clipboardFileFormat(long bool) []byte {
	if !long {
		name := make([]byte, 32)
		copy(name, clipFileGroupName) // Short format names use CB_ASCII_NAMES (0x4).
		return join(u32(clipFileGroupDescriptor), name)
	}
	name, _ := clipboardEncodeText(clipFileGroupName)
	return join(u32(clipFileGroupDescriptor), name)
}
