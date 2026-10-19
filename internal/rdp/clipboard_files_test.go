package rdp

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"anykvm/internal/clipboard"
)

type memoryFileClipboard struct {
	memoryClipboard
	paths []string
}

func (b *memoryFileClipboard) ReadFiles() ([]string, bool, uint64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.paths...), len(b.paths) != 0, b.revision, nil
}

func fileClipboardTestClient(t *testing.T, paths []string, direction clipboard.Direction) (*client, *memoryFileClipboard, *pointerRecordingConn) {
	t.Helper()
	c, _, conn := clipboardTestClient(t)
	b := &memoryFileClipboard{memoryClipboard: memoryClipboard{revision: 1}, paths: paths}
	c.s.cfg.Clipboard = b
	c.s.cfg.ClipboardOptions = &clipboard.Options{Files: direction}
	c.clipboard.peerReady, c.clipboard.peerFiles, c.clipboard.longNames = true, true, true
	t.Cleanup(func() {
		select {
		case <-c.done:
		default:
			close(c.done)
		}
		c.s.wg.Wait()
		c.cleanupClipboardFiles()
	})
	return c, b, conn
}

func writeClipboardTestFile(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func clipboardFileRequest(stream, index, flags uint32, offset uint64, size uint32) []byte {
	return join(u32(stream), u32(index), u32(flags), u32(uint32(offset)), u32(uint32(offset>>32)), u32(size))
}

func sparseClipboardTestFile(t *testing.T, size int64) string {
	t.Helper()
	path := writeClipboardTestFile(t, "large.bin", nil)
	if err := os.Truncate(path, size); err != nil {
		t.Fatal(err)
	}
	return path
}

func negotiateClipboardFilePeer(t *testing.T, c *client, huge bool) {
	t.Helper()
	flags := uint32(2 | clipStreamFiles | clipNoFilePaths)
	if huge {
		flags |= 0x20 // A peer's huge-file flag must not override our fixed limit.
	}
	c.clipboard.peerReady = false
	if err := c.handleClipboard(clipPDU(clipCapabilities, 0, join(u16(1), u16(0), u16(1), u16(12), u32(2), u32(flags)))); err != nil {
		t.Fatal(err)
	}
	c.clipboard.peerReady = true
	if !c.clipboard.peerFiles {
		t.Fatal("file streaming capability was not decoded")
	}
}

func TestClipboardFileLimitRanges(t *testing.T) {
	const size = maxClipboardFileBytes
	path := sparseClipboardTestFile(t, size)
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Read across the former 1 GiB limit and at the last legal file bytes.
	offsets := []int64{1<<30 - 3, 1<<30 + 17, size - 8}
	content := []byte("boundary")
	for _, offset := range offsets {
		if _, err := file.WriteAt(content, offset); err != nil {
			file.Close()
			t.Fatal(err)
		}
	}
	file.Close()
	c, _, conn := fileClipboardTestClient(t, []string{path}, clipboard.ServerToClient)
	negotiateClipboardFilePeer(t, c, true)
	acceptClipboardFileOffer(t, c, conn)
	descriptor := c.clipboard.localFiles.data[4:]
	if got := uint64(le.Uint32(descriptor[64:]))<<32 | uint64(le.Uint32(descriptor[68:])); got != uint64(size) {
		t.Fatalf("64-bit descriptor size: %d", got)
	}
	if err := c.handleClipboard(clipPDU(clipFileContentsRequest, 0, clipboardFileRequest(9, 0, clipFileSize, 0, 8))); err != nil {
		t.Fatal(err)
	}
	if p := clipboardPackets(t, conn); len(p) != 1 || le.Uint64(p[0][12:]) != uint64(size) {
		t.Fatal("64-bit file size response is incorrect")
	}
	for _, offset := range offsets {
		if err := c.handleClipboard(clipPDU(clipFileContentsRequest, 0, clipboardFileRequest(10, 0, clipFileRange, uint64(offset), 8))); err != nil {
			t.Fatal(err)
		}
		if p := clipboardPackets(t, conn); len(p) != 1 || le.Uint16(p[0][2:]) != clipOK || !bytes.Equal(p[0][12:], content) {
			t.Fatalf("64-bit range failed at offset %d", offset)
		}
	}
	for _, offset := range []uint64{uint64(size) + 1, 1 << 32} {
		if err := c.handleClipboard(clipPDU(clipFileContentsRequest, 0, clipboardFileRequest(11, 0, clipFileRange, offset, 8))); err != nil {
			t.Fatal(err)
		}
		if p := clipboardPackets(t, conn); len(p) != 1 || le.Uint16(p[0][2:]) != clipFail {
			t.Fatal("out-of-bounds file range was served")
		}
	}
}

func TestClipboardLargeFilesPeerCompatibility(t *testing.T) {
	for _, size := range []int64{1<<30 + 1, maxClipboardFileBytes, maxClipboardFileBytes + 1, 5 << 30} {
		path := sparseClipboardTestFile(t, size)
		for _, huge := range []bool{false, true} {
			t.Run(fmt.Sprintf("size=%d/huge=%t", size, huge), func(t *testing.T) {
				c, _, conn := fileClipboardTestClient(t, []string{path}, clipboard.ServerToClient)
				negotiateClipboardFilePeer(t, c, huge)
				if err := c.pollClipboard(); err != nil {
					t.Fatal(err)
				}
				clipboardPackets(t, conn)
				want := size <= maxClipboardFileBytes
				if (c.clipboard.localFiles != nil) != want {
					t.Fatal("file offer does not respect the fixed single-file limit")
				}
			})
		}
	}
}

func TestClipboardFilesCombinedSizeAboveLimit(t *testing.T) {
	first := sparseClipboardTestFile(t, 1<<30+1)
	second := sparseClipboardTestFile(t, 1<<30+1)
	otherName := filepath.Join(filepath.Dir(second), "other.bin")
	if err := os.Rename(second, otherName); err != nil {
		t.Fatal(err)
	}
	c, _, conn := fileClipboardTestClient(t, []string{first, otherName}, clipboard.ServerToClient)
	acceptClipboardFileOffer(t, c, conn)
	files := c.clipboard.localFiles
	if len(files.entries) != 2 || files.entries[0].info.Size()+files.entries[1].info.Size() <= maxClipboardFileBytes {
		t.Fatal("valid files whose combined size exceeds 2 GiB were not advertised")
	}
}

func acceptClipboardFileOffer(t *testing.T, c *client, conn *pointerRecordingConn) {
	t.Helper()
	if err := c.pollClipboard(); err != nil {
		t.Fatal(err)
	}
	p := clipboardPackets(t, conn)
	if len(p) != 1 || le.Uint16(p[0]) != clipFormatList || le.Uint32(p[0][8:]) != clipFileGroupDescriptor {
		t.Fatalf("expected file format announcement, got %x", p)
	}
	if c.clipboard.longNames {
		name, err := clipboardDecodeText(p[0][12:])
		if err != nil || name != clipFileGroupName || le.Uint16(p[0][2:]) != 0 {
			t.Fatal("incorrect Unicode file format name")
		}
	} else if len(p[0]) != 44 || string(bytes.TrimRight(p[0][12:], "\x00")) != clipFileGroupName || le.Uint16(p[0][2:]) != 4 {
		t.Fatal("incorrect short ASCII file format name")
	}
	if err := c.handleClipboard(clipPDU(clipFormatListResponse, clipOK, nil)); err != nil {
		t.Fatal(err)
	}
}

func TestClipboardFilesOnDemand(t *testing.T) {
	content := bytes.Repeat([]byte("文件内容🧩"), 80000)
	path := writeClipboardTestFile(t, "报告🧩.txt", content)
	for _, long := range []bool{false, true} {
		t.Run(fmt.Sprint(long), func(t *testing.T) {
			c, _, conn := fileClipboardTestClient(t, []string{path}, 2)
			c.clipboard.longNames = long
			acceptClipboardFileOffer(t, c, conn)
			// A copied file only emits a format announcement; asking for its list
			// still returns metadata, never the file bytes or an absolute path.
			if err := c.handleClipboard(clipPDU(clipDataRequest, 0, u32(clipFileGroupDescriptor))); err != nil {
				t.Fatal(err)
			}
			p := clipboardPackets(t, conn)
			if len(p) != 1 || le.Uint16(p[0][2:]) != clipOK || len(p[0]) != 12+clipDescriptorSize {
				t.Fatal("invalid file descriptor reply")
			}
			d := p[0][12:]
			name, err := clipboardDecodeText(d[72:])
			if err != nil || name != filepath.Base(path) || le.Uint32(d[36:]) != 0x80 ||
				le.Uint32(d[68:]) != uint32(len(content)) || bytes.Contains(p[0], content[:100]) {
				t.Fatal("file descriptor is incorrect or includes file content")
			}
			if err := c.handleClipboard(clipPDU(clipFileContentsRequest, 0, clipboardFileRequest(17, 0, clipFileSize, 0, 8))); err != nil {
				t.Fatal(err)
			}
			p = clipboardPackets(t, conn)
			if len(p) != 1 || le.Uint16(p[0]) != clipFileContentsResponse || le.Uint16(p[0][2:]) != clipOK ||
				le.Uint32(p[0][8:]) != 17 || le.Uint64(p[0][12:]) != uint64(len(content)) {
				t.Fatal("file size response is incorrect")
			}
			var received []byte
			for offset := 0; offset < len(content); offset += 65536 {
				if err := c.handleClipboard(clipPDU(clipFileContentsRequest, 0, clipboardFileRequest(18, 0, clipFileRange, uint64(offset), 65536))); err != nil {
					t.Fatal(err)
				}
				p = clipboardPackets(t, conn)
				if len(p) != 1 || le.Uint16(p[0][2:]) != clipOK || le.Uint32(p[0][8:]) != 18 {
					t.Fatal("file range response is incorrect")
				}
				received = append(received, p[0][12:]...)
			}
			if !bytes.Equal(received, content) {
				t.Fatal("fragmented file transfer changed the bytes")
			}
			if err := c.handleClipboard(clipPDU(clipFileContentsRequest, 0, clipboardFileRequest(19, 0, clipFileRange, 0, 1<<20))); err != nil {
				t.Fatal(err)
			}
			p = clipboardPackets(t, conn)
			if len(p) != 1 || le.Uint16(p[0][2:]) != clipOK || !bytes.Equal(p[0][12:], content[:1<<20]) {
				t.Fatal("full 1 MiB Windows read failed or was truncated")
			}
		})
	}
}

func TestClipboardFileDirectionsAndCapabilities(t *testing.T) {
	path := writeClipboardTestFile(t, "file.txt", []byte("secret"))
	for _, direction := range []clipboard.Direction{0, 1, 2, 3} {
		for _, masterOff := range []bool{false, true} {
			for _, peerFiles := range []bool{false, true} {
				t.Run(fmt.Sprintf("%d/off=%t/peer=%t", direction, masterOff, peerFiles), func(t *testing.T) {
					c, b, conn := fileClipboardTestClient(t, []string{path}, direction)
					c.s.cfg.DisableClipboard = masterOff
					c.clipboard.started = false
					if err := c.startClipboard(); err != nil {
						t.Fatal(err)
					}
					enabled := !masterOff && direction.Allows(clipboard.ServerToClient)
					p := clipboardPackets(t, conn)
					if (len(p) == 2) != enabled || c.clipboard.started != enabled {
						t.Fatal("file policy did not control initialization")
					}
					if enabled && le.Uint32(p[0][20:]) != 2|clipStreamFiles|clipNoFilePaths {
						t.Fatal("incorrect file capabilities or advertised unsupported locking")
					}
					c.clipboard.mu.Lock()
					c.clipboard.peerFiles = peerFiles
					c.clipboard.mu.Unlock()
					if err := c.pollClipboard(); err != nil {
						t.Fatal(err)
					}
					p = clipboardPackets(t, conn)
					if enabled && peerFiles {
						if len(p) != 1 || le.Uint32(p[0][8:]) != clipFileGroupDescriptor {
							t.Fatal("file format not offered with supported direction")
						}
					} else if c.clipboard.localFiles != nil {
						t.Fatal("files offered without policy or peer support")
					}
					// No incoming file list/content is ever requested or published.
					clipboardPackets(t, conn)
					if err := c.handleClipboard(clipPDU(clipFormatList, 0, clipboardFileFormat(true))); err != nil {
						t.Fatal(err)
					}
					p = clipboardPackets(t, conn)
					if len(p) > 1 || b.writes != 0 {
						t.Fatal("unimplemented receive direction downloaded or wrote files")
					}
				})
			}
		}
	}
	backend := &memoryFileClipboard{}
	for _, direction := range []clipboard.Direction{0, 1, 2, 3} {
		s, err := NewServer(Config{
			Addr: "127.0.0.1:3389", Username: "test", Password: "test",
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{{}}},
			Clipboard: backend, ClipboardOptions: &clipboard.Options{Files: direction},
		})
		if err != nil || (s.cfg.Clipboard != nil) != direction.Allows(clipboard.ServerToClient) {
			t.Fatal("file-only backend selection", direction, err)
		}
	}
}

func TestClipboardFileTreesAndEmptyFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "目录")
	if err := os.MkdirAll(filepath.Join(root, "空目录"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "empty.txt"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	files, err := snapshotClipboardFiles([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer files.close()
	want := []string{"目录", "目录\\empty.txt", "目录\\空目录"}
	if len(files.entries) != len(want) {
		t.Fatal("missing directory entries")
	}
	for i, entry := range files.entries {
		if entry.name != want[i] || (entry.file == nil) != (i != 1) {
			t.Fatal("incorrect nested directory/file descriptor", entry.name)
		}
	}
	c, _, conn := fileClipboardTestClient(t, []string{root}, 2)
	acceptClipboardFileOffer(t, c, conn)
	for _, index := range []uint32{0, 1, 2} {
		if err := c.handleClipboard(clipPDU(clipFileContentsRequest, 0, clipboardFileRequest(8, index, clipFileRange, 0, 1024))); err != nil {
			t.Fatal(err)
		}
		p := clipboardPackets(t, conn)
		flag := uint16(clipFail)
		if index == 1 {
			flag = clipOK
		}
		if len(p) != 1 || le.Uint16(p[0][2:]) != flag || len(p[0]) != 12 {
			t.Fatal("directory or empty file range reply is incorrect")
		}
	}
}

func TestClipboardFileRequestFailures(t *testing.T) {
	path := writeClipboardTestFile(t, "file.txt", []byte("private"))
	for _, request := range [][]byte{
		clipboardFileRequest(42, ^uint32(0), clipFileRange, 0, 8),
		clipboardFileRequest(42, 0, 3, 0, 8),
		clipboardFileRequest(42, 0, clipFileSize, 1, 8),
		clipboardFileRequest(42, 0, clipFileSize, 0, 7),
		clipboardFileRequest(42, 0, clipFileRange, 0, maxClipboardFileChunk+1),
		clipboardFileRequest(42, 0, clipFileRange, 1<<32, 8),
		join(clipboardFileRequest(42, 0, clipFileRange, 0, 8), u32(12)), // Unnegotiated lock ID.
		u32(42),
	} {
		c, _, conn := fileClipboardTestClient(t, []string{path}, 2)
		acceptClipboardFileOffer(t, c, conn)
		if err := c.handleClipboard(clipPDU(clipFileContentsRequest, 0, request)); err != nil {
			t.Fatal(err)
		}
		p := clipboardPackets(t, conn)
		if len(p) != 1 || le.Uint16(p[0][2:]) != clipFail || len(p[0]) != 12 || le.Uint32(p[0][8:]) != 42 {
			t.Fatalf("malformed request leaked file content: %x", request)
		}
	}
	for _, change := range []string{"clipboard", "file", "reject", "unaccepted", "remote"} {
		t.Run(change, func(t *testing.T) {
			path := writeClipboardTestFile(t, "file.txt", []byte("private"))
			c, b, conn := fileClipboardTestClient(t, []string{path}, 3)
			acceptClipboardFileOffer(t, c, conn)
			switch change {
			case "clipboard":
				b.mu.Lock()
				b.revision++
				b.mu.Unlock()
			case "file":
				if err := os.WriteFile(path, []byte("changed and longer"), 0600); err != nil {
					t.Fatal(err)
				}
			case "reject", "unaccepted":
				c.clipboard.listPending, c.clipboard.listAccepted = true, false
				if change == "reject" {
					if err := c.handleClipboard(clipPDU(clipFormatListResponse, clipFail, nil)); err != nil {
						t.Fatal(err)
					}
				}
			case "remote":
				c.s.cfg.ClipboardOptions.Text = 1
				if err := c.handleClipboard(clipPDU(clipFormatList, 0, nil)); err != nil {
					t.Fatal(err)
				}
				clipboardPackets(t, conn)
			}
			for _, kind := range []int{clipDataRequest, clipFileContentsRequest} {
				data := u32(clipFileGroupDescriptor)
				if kind == clipFileContentsRequest {
					data = clipboardFileRequest(8, 0, clipFileRange, 0, 8)
				}
				// Descriptor size/mtime is a snapshot; content reads additionally
				// reject changed files, even when the pasteboard itself is unchanged.
				if change == "file" && kind == clipDataRequest {
					continue
				}
				if err := c.handleClipboard(clipPDU(kind, 0, data)); err != nil {
					t.Fatal(err)
				}
				p := clipboardPackets(t, conn)
				if len(p) != 1 || le.Uint16(p[0][2:]) != clipFail {
					t.Fatal("stale/rejected file offer leaked bytes", change, kind)
				}
			}
		})
	}
}

func TestClipboardFileSnapshotSafetyAndCleanup(t *testing.T) {
	path := writeClipboardTestFile(t, "file.txt", []byte("original"))
	files, err := snapshotClipboardFiles([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	// An atomic replacement must not redirect an already advertised file index.
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 8)
	if _, err := files.entries[0].file.ReadAt(data, 0); err != nil || string(data) != "original" {
		t.Fatal("snapshot did not pin the selected file")
	}
	files.close()
	if _, err := files.entries[0].file.ReadAt(data, 0); err == nil {
		t.Fatal("file handle remained open after cleanup")
	}
	for _, name := range []string{"../escape", "/absolute", "C:/drive", "a\\b", "nul.txt", "CON", "COM1", "a:", "a.", "a ", "a//b", "a/../b", strings.Repeat("长", 260)} {
		if err := validateClipboardFileName(name); err == nil {
			t.Fatal("accepted unsafe clipboard path", name)
		}
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, paths := range [][]string{{link}, {path, path}, {"relative"}} {
		if files, err := snapshotClipboardFiles(paths); err == nil {
			files.close()
			t.Fatal("accepted ambiguous or unsafe selection", paths)
		}
	}
	c, _, conn := fileClipboardTestClient(t, []string{path}, 2)
	c.clipboard.started = false
	if err := c.startClipboard(); err != nil {
		t.Fatal(err)
	}
	clipboardPackets(t, conn)
	acceptClipboardFileOffer(t, c, conn)
	file := c.clipboard.localFiles.entries[0].file
	close(c.done)
	done := make(chan struct{})
	go func() { c.s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("file clipboard worker did not stop")
	}
	if _, err := file.ReadAt(data, 0); err == nil || c.clipboard.localFiles != nil {
		t.Fatal("disconnected session retained clipboard file handles")
	}
}
