package rdp

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"anykvm/internal/capture"
	"anykvm/internal/clipboard"
)

type deferredMemoryClipboard struct {
	memoryClipboard
	files   []capture.ClipboardFile
	receive func(context.Context, int, *os.File) error
	retired int
}

type pendingMemoryFileClipboard struct {
	memoryFileClipboard
	pending bool
}

func (b *pendingMemoryFileClipboard) ReadFiles() ([]string, bool, uint64, error) {
	if b.pending {
		return nil, false, b.revision, capture.ErrClipboardFilesPending
	}
	return b.memoryFileClipboard.ReadFiles()
}

func TestClipboardPendingFilesAreNotAdvertisedToOtherSessions(t *testing.T) {
	path := writeClipboardTestFile(t, "staged.txt", []byte("complete"))
	c, _, conn := clipboardTestClient(t)
	b := &pendingMemoryFileClipboard{memoryFileClipboard: memoryFileClipboard{
		memoryClipboard: memoryClipboard{revision: 1}, paths: []string{path}}, pending: true}
	c.s.cfg.Clipboard, c.s.cfg.ClipboardOptions = b, &clipboard.Options{Files: clipboard.ServerToClient}
	c.clipboard.peerReady, c.clipboard.peerFiles, c.clipboard.longNames = true, true, true
	t.Cleanup(c.cleanupClipboardFiles)
	if err := c.pollClipboard(); err != nil || len(clipboardPackets(t, conn)) != 0 || c.clipboard.haveRevision {
		t.Fatal("pending file was advertised or its revision consumed", err)
	}
	b.pending = false // Materializing content does not change pasteboard revision.
	acceptClipboardFileOffer(t, c, conn)
	if c.clipboard.localFiles.entries[0].info.Size() != int64(len("complete")) {
		t.Fatal("completed file size was not advertised")
	}
}

func (b *deferredMemoryClipboard) WriteDeferredFiles(ctx context.Context, revision uint64, files []capture.ClipboardFile,
	receive func(context.Context, int, *os.File) error) (uint64, func(), error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if revision != b.revision {
		return b.revision, nil, fmt.Errorf("clipboard changed")
	}
	b.files, b.receive = files, receive
	b.available, b.text = false, ""
	b.revision++
	return b.revision, func() { b.mu.Lock(); b.retired++; b.mu.Unlock() }, nil
}

func receivingClipboardClient(t *testing.T, direction clipboard.Direction) (*client, *deferredMemoryClipboard, *pointerRecordingConn) {
	t.Helper()
	c, _, conn := clipboardTestClient(t)
	b := &deferredMemoryClipboard{memoryClipboard: memoryClipboard{revision: 1}}
	c.s.cfg.Clipboard, c.s.cfg.ClipboardOptions = b, &clipboard.Options{Files: direction}
	c.clipboard.peerReady, c.clipboard.peerFiles, c.clipboard.longNames = true, true, true
	t.Cleanup(c.cleanupClipboardFiles)
	return c, b, conn
}

func remoteClipboardFileOffer(long bool, id uint32) []byte {
	p := clipboardFileFormat(long)
	le.PutUint32(p, id)
	return p
}

func publishTestRemoteFiles(t *testing.T, c *client, conn *pointerRecordingConn, data []byte) {
	t.Helper()
	if err := c.handleClipboard(clipPDU(clipFormatList, 0, remoteClipboardFileOffer(true, 0xc127))); err != nil {
		t.Fatal(err)
	}
	p := clipboardPackets(t, conn)
	if len(p) != 2 || le.Uint16(p[1]) != clipDataRequest || le.Uint32(p[1][8:]) != 0xc127 {
		t.Fatal("remote registered file format was not requested", p)
	}
	if err := c.handleClipboard(clipPDU(clipDataResponse, clipOK, data)); err != nil {
		t.Fatal(err)
	}
	if len(clipboardPackets(t, conn)) != 0 {
		t.Fatal("file content requested before a paste")
	}
}

func waitClipboardFileRequest(t *testing.T, conn *pointerRecordingConn) []byte {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for len(conn.packets) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	p := clipboardPackets(t, conn)
	if len(p) != 1 || le.Uint16(p[0]) != clipFileContentsRequest {
		t.Fatal("expected one file read", p)
	}
	return p[0][8:]
}

func TestClipboardReceiveFilesDeferred(t *testing.T) {
	root := t.TempDir()
	contents := bytes.Repeat([]byte("remote 文件🧩\n"), 50000)
	if err := os.WriteFile(filepath.Join(root, "报告.txt"), contents, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "empty"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "empty-dir"), 0700); err != nil {
		t.Fatal(err)
	}
	list, err := snapshotClipboardFiles([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer list.close()
	c, b, conn := receivingClipboardClient(t, clipboard.ClientToServer)
	publishTestRemoteFiles(t, c, conn, list.data)
	if len(b.files) != len(list.entries) || b.receive == nil {
		t.Fatal("placeholders not published")
	}
	if err := c.pollClipboard(); err != nil || len(clipboardPackets(t, conn)) != 0 {
		t.Fatal("placeholder clipboard echoed or downloaded", err)
	}
	for index, entry := range list.entries {
		if entry.file == nil {
			continue
		}
		file, err := os.CreateTemp(t.TempDir(), "received-")
		if err != nil {
			t.Fatal(err)
		}
		finished := make(chan error, 1)
		go func(index int) { finished <- b.receive(context.Background(), index, file) }(index)
		for offset := int64(0); offset < entry.info.Size(); {
			request := waitClipboardFileRequest(t, conn)
			if len(request) != 24 || le.Uint32(request[4:]) != uint32(index) || le.Uint32(request[8:]) != clipFileRange || le.Uint64(request[12:]) != uint64(offset) {
				t.Fatal("wrong file index/range", request)
			}
			length := le.Uint32(request[20:])
			if length > clipboardReceiveChunk {
				t.Fatal("unbounded file read")
			}
			// Short but nonempty responses are legal and must advance exactly
			// by the returned length, without skipping any remote bytes.
			length = min(length, 50000)
			chunk := make([]byte, length)
			if n, err := entry.file.ReadAt(chunk, offset); err != nil && err != io.EOF || n != len(chunk) {
				t.Fatal(n, err)
			}
			if err := c.handleClipboard(clipPDU(clipFileContentsResponse, clipOK, join(request[:4], chunk))); err != nil {
				t.Fatal(err)
			}
			offset += int64(len(chunk))
		}
		select {
		case err := <-finished:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("file read deadlocked")
		}
		got, _ := os.ReadFile(file.Name())
		want := make([]byte, entry.info.Size())
		if n, err := entry.file.ReadAt(want, 0); err != nil && err != io.EOF || n != len(want) {
			t.Fatal(n, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatal("received file content differs")
		}
		file.Close()
	}
	if err := c.handleClipboard(clipPDU(clipFileContentsResponse, clipOK, join(u32(9999), []byte("unsolicited")))); err != nil {
		t.Fatal(err)
	}
	_, _ = b.WriteText("new host copy")
	if err := c.pollClipboard(); err != nil {
		t.Fatal(err)
	}
	if b.retired != 1 || c.clipboard.remoteFiles != nil {
		t.Fatal("host clipboard replacement did not retire files")
	}
}

func TestClipboardReceiveFilesCancellationAndLateReplies(t *testing.T) {
	path := writeClipboardTestFile(t, "data.txt", []byte("original"))
	list, err := snapshotClipboardFiles([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	defer list.close()
	for _, disconnect := range []bool{false, true} {
		t.Run(fmt.Sprint(disconnect), func(t *testing.T) {
			c, b, conn := receivingClipboardClient(t, clipboard.ClientToServer)
			publishTestRemoteFiles(t, c, conn, list.data)
			file, err := os.CreateTemp(t.TempDir(), "received-")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			finished := make(chan error, 1)
			go func() { finished <- b.receive(context.Background(), 0, file) }()
			request := waitClipboardFileRequest(t, conn)
			if disconnect {
				close(c.done)
				c.cleanupClipboardFiles()
			} else {
				if err := c.handleClipboard(clipPDU(clipFormatList, 0, nil)); err != nil {
					t.Fatal(err)
				}
				clipboardPackets(t, conn)
			}
			select {
			case err := <-finished:
				if err == nil {
					t.Fatal("cancelled read succeeded")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("cancelled transfer deadlocked")
			}
			if err := c.handleClipboard(clipPDU(clipFileContentsResponse, clipOK, join(request[:4], []byte("original")))); err != nil {
				t.Fatal(err)
			}
			if info, _ := file.Stat(); info.Size() != 0 {
				t.Fatal("late response wrote expired file")
			}
			if b.retired != 1 {
				t.Fatal("offer was not retired exactly once")
			}
		})
	}
}

func TestClipboardReceiveFilesUnknownSizeAndFailure(t *testing.T) {
	path := writeClipboardTestFile(t, "data.txt", []byte("original"))
	list, err := snapshotClipboardFiles([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	defer list.close()
	data := append([]byte(nil), list.data...)
	le.PutUint32(data[4:], le.Uint32(data[4:]) & ^uint32(0x40))
	c, b, conn := receivingClipboardClient(t, clipboard.ClientToServer)
	publishTestRemoteFiles(t, c, conn, data)
	file, err := os.CreateTemp(t.TempDir(), "received-")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	finished := make(chan error, 1)
	go func() { finished <- b.receive(context.Background(), 0, file) }()
	request := waitClipboardFileRequest(t, conn)
	if le.Uint32(request[8:]) != clipFileSize || le.Uint32(request[20:]) != 8 {
		t.Fatal("unknown size was not requested at paste")
	}
	size := make([]byte, 8)
	le.PutUint64(size, 8)
	if err := c.handleClipboard(clipPDU(clipFileContentsResponse, clipOK, join(request[:4], size))); err != nil {
		t.Fatal(err)
	}
	request = waitClipboardFileRequest(t, conn)
	if err := c.handleClipboard(clipPDU(clipFileContentsResponse, clipFail, request[:4])); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("failed file read succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("failed read did not return")
	}
}

func TestClipboardReceiveFilesWindowsPadding(t *testing.T) {
	contents := []byte{'a', 0, 0}
	path := writeClipboardTestFile(t, "three-bytes.txt", contents)
	list, err := snapshotClipboardFiles([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	defer list.close()
	for _, unknownSize := range []bool{false, true} {
		for _, padding := range []struct {
			name string
			data []byte
		}{
			{"none", nil}, {"zero", u32(0)}, {"nonzero", u32(0x12345678)}, {"all-bits", u32(0xffffffff)},
		} {
			t.Run(fmt.Sprintf("unknown=%t/padding=%s", unknownSize, padding.name), func(t *testing.T) {
				c, b, conn := receivingClipboardClient(t, clipboard.ClientToServer)
				data := append([]byte(nil), list.data...)
				if unknownSize {
					le.PutUint32(data[4:], le.Uint32(data[4:]) & ^uint32(0x40))
				}
				publishTestRemoteFiles(t, c, conn, data)
				file, err := os.CreateTemp(t.TempDir(), "received-")
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				finished := make(chan error, 1)
				go func() { finished <- b.receive(context.Background(), 0, file) }()
				reply := func(request, bytes []byte) {
					t.Helper()
					// mstsc's unused DWORD outside dataLen may contain nonzero
					// bytes. Split the suffix across static-channel fragments.
					pdu := join(clipPDU(clipFileContentsResponse, clipOK, join(request[:4], bytes)), padding.data)
					split := len(pdu) - 2
					for i, fragment := range [][]byte{pdu[:split], pdu[split:]} {
						flags := uint32(1)
						if i == 1 {
							flags = 2
						}
						if err := c.handleClipboardVC(join(u32(uint32(len(pdu))), u32(flags), fragment)); err != nil {
							t.Fatal(err)
						}
					}
				}
				request := waitClipboardFileRequest(t, conn)
				if unknownSize {
					if le.Uint32(request[8:]) != clipFileSize {
						t.Fatal("missing file size request")
					}
					size := make([]byte, 8)
					le.PutUint64(size, uint64(len(contents)))
					reply(request, size)
					request = waitClipboardFileRequest(t, conn)
				}
				reply(request, contents)
				select {
				case err := <-finished:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("padded response stalled file transfer")
				}
				got, err := os.ReadFile(file.Name())
				if err != nil || !bytes.Equal(got, contents) {
					t.Fatalf("file bytes changed by transport padding: %x (%v)", got, err)
				}
				if err := c.handleClipboard(clipPDU(clipFormatList, 0, nil)); err != nil {
					t.Fatal("clipboard channel failed after the transfer", err)
				}
				if packets := clipboardPackets(t, conn); len(packets) != 1 || le.Uint16(packets[0]) != clipFormatListResponse {
					t.Fatal("clipboard channel did not acknowledge the next copy")
				}
			})
		}
	}
}

func TestClipboardFileResponseRejectsInvalidPadding(t *testing.T) {
	pdu := clipPDU(clipFileContentsResponse, clipOK, join(u32(999), []byte("abc")))
	truncated := append([]byte(nil), pdu...)
	le.PutUint32(truncated[4:], le.Uint32(truncated[4:])+1)
	for _, packet := range [][]byte{
		truncated, join(pdu, make([]byte, 8)), join(pdu, []byte{0}),
		join(clipPDU(clipDataResponse, clipOK, u16(0)), u32(0)),
	} {
		c, _, _ := clipboardTestClient(t)
		if err := c.handleClipboardVC(join(u32(uint32(len(packet))), u32(3), packet)); err == nil {
			t.Fatal("accepted an invalid length or non-file padding")
		}
	}
}

func TestClipboardReceiveFileDirections(t *testing.T) {
	for _, direction := range []clipboard.Direction{0, 1, 2, 3} {
		for _, off := range []bool{false, true} {
			c, b, conn := receivingClipboardClient(t, direction)
			c.s.cfg.DisableClipboard = off
			if err := c.handleClipboard(clipPDU(clipFormatList, 0, remoteClipboardFileOffer(true, 0xc199))); err != nil {
				t.Fatal(err)
			}
			p := clipboardPackets(t, conn)
			allowed := !off && direction.Allows(clipboard.ClientToServer)
			if (len(p) == 2) != allowed || b.receive != nil {
				t.Fatal("incoming file direction was not enforced", direction, off, p)
			}
		}
	}
	b := &deferredMemoryClipboard{}
	s, err := NewServer(Config{Addr: "127.0.0.1:3389", Username: "test", Password: "test",
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{{}}}, Clipboard: b, ClipboardOptions: &clipboard.Options{Files: clipboard.ClientToServer}})
	if err != nil || s.cfg.Clipboard != b {
		t.Fatal("receive-only backend not retained", err)
	}
}

func TestClipboardReceiveFileListValidation(t *testing.T) {
	path := writeClipboardTestFile(t, "safe.txt", []byte("safe"))
	list, err := snapshotClipboardFiles([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	defer list.close()
	for _, name := range []string{"..\\escape", "C:\\escape", "safe.txt\\child", "safe.txt", "SAFE.TXT", ".lock"} {
		data := append([]byte(nil), list.data...)
		entry := append([]byte(nil), data[4:]...)
		clear(entry[72:])
		encoded, _ := clipboardEncodeText(name)
		copy(entry[72:], encoded)
		data = append(data, entry...)
		le.PutUint32(data, 2)
		if _, err := decodeClipboardFileList(data); err == nil {
			t.Fatal("unsafe file list accepted", name)
		}
	}
	for _, long := range []bool{false, true} {
		p := remoteClipboardFileOffer(long, 0xc199)
		id, err := clipboardRemoteFileFormat(p, long, !long)
		if err != nil || id != 0xc199 {
			t.Fatal("remote file format ID/name mapping failed", id, err)
		}
	}
	data := append([]byte(nil), list.data...)
	le.PutUint32(data[4+64:], 0x80000000)
	if _, err := decodeClipboardFileList(data); err == nil {
		t.Fatal("oversized file accepted")
	}
	if _, err := decodeClipboardFileList(list.data[:len(list.data)-1]); err == nil {
		t.Fatal("truncated file list accepted")
	}
}

func remoteClipboardSizedFiles(t *testing.T, sizes ...uint64) []byte {
	t.Helper()
	data := make([]byte, 4+len(sizes)*clipDescriptorSize)
	le.PutUint32(data, uint32(len(sizes)))
	for index, size := range sizes {
		d := data[4+index*clipDescriptorSize:][:clipDescriptorSize]
		le.PutUint32(d, 4|0x40)
		le.PutUint32(d[36:], 0x80)
		le.PutUint32(d[64:], uint32(size>>32))
		le.PutUint32(d[68:], uint32(size))
		name, err := clipboardEncodeText(fmt.Sprintf("file-%d.bin", index))
		if err != nil {
			t.Fatal(err)
		}
		copy(d[72:], name)
	}
	return data
}

func TestClipboardReceiveLargeFileLists(t *testing.T) {
	for _, sizes := range [][]uint64{{1<<30 + 1}, {1<<30 + 1, 1<<30 + 1}, {uint64(maxClipboardFileBytes), uint64(maxClipboardFileBytes)}} {
		data := remoteClipboardSizedFiles(t, sizes...)
		files, err := decodeClipboardFileList(data)
		if err != nil || len(files) != len(sizes) {
			t.Fatal("large file metadata rejected", err)
		}
		for _, huge := range []bool{false, true} {
			c, b, conn := receivingClipboardClient(t, clipboard.ClientToServer)
			negotiateClipboardFilePeer(t, c, huge)
			publishTestRemoteFiles(t, c, conn, data)
			if b.receive == nil {
				t.Fatal("incoming collection was limited by its combined size")
			}
			if len(clipboardPackets(t, conn)) != 0 {
				t.Fatal("large file was downloaded before a paste")
			}
		}
	}
	for _, sizes := range [][]uint64{{1 << 63}, {uint64(maxClipboardFileBytes) + 1}, {5 << 30}} {
		data := remoteClipboardSizedFiles(t, sizes...)
		if _, err := decodeClipboardFileList(data); err == nil {
			t.Fatal("oversized individual file accepted")
		}
		c, b, conn := receivingClipboardClient(t, clipboard.ClientToServer)
		negotiateClipboardFilePeer(t, c, true)
		publishTestRemoteFiles(t, c, conn, data)
		if b.receive != nil || c.clipboard.remoteFiles != nil {
			t.Fatal("oversized incoming file was published")
		}
	}
}

func TestClipboardReceiveLargeFileRangeOffset(t *testing.T) {
	c, _, conn := receivingClipboardClient(t, clipboard.ClientToServer)
	negotiateClipboardFilePeer(t, c, true)
	publishTestRemoteFiles(t, c, conn, remoteClipboardSizedFiles(t, uint64(maxClipboardFileBytes)))
	remote := c.clipboard.remoteFiles
	const offset = uint64(1<<30 + 123)
	finished := make(chan clipboardFileReply, 1)
	go func() {
		data, err := c.requestClipboardFileChunk(context.Background(), remote, 0, clipFileRange, offset, 4)
		finished <- clipboardFileReply{data: data, err: err}
	}()
	request := waitClipboardFileRequest(t, conn)
	if le.Uint32(request[12:]) != uint32(offset) || le.Uint32(request[16:]) != 0 {
		t.Fatal("high file offset was truncated")
	}
	if err := c.handleClipboard(clipPDU(clipFileContentsResponse, clipOK, join(request[:4], []byte("tail")))); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-finished:
		if result.err != nil || string(result.data) != "tail" {
			t.Fatal("high-offset response failed", result.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("high-offset request stalled")
	}
}

func TestClipboardReceiveSingleFileLimitUnknownSize(t *testing.T) {
	for _, tc := range []struct {
		size uint64
		huge bool
		ok   bool
	}{{uint64(maxClipboardFileBytes), false, true}, {uint64(maxClipboardFileBytes) + 1, false, false},
		{uint64(maxClipboardFileBytes), true, true}, {uint64(maxClipboardFileBytes) + 1, true, false}, {1 << 63, true, false}} {
		t.Run(fmt.Sprintf("%d/%t", tc.size, tc.huge), func(t *testing.T) {
			c, b, conn := receivingClipboardClient(t, clipboard.ClientToServer)
			negotiateClipboardFilePeer(t, c, tc.huge)
			data := remoteClipboardSizedFiles(t, 0, uint64(maxClipboardFileBytes))
			le.PutUint32(data[4:], 4) // File size is not known until paste.
			publishTestRemoteFiles(t, c, conn, data)
			file, err := os.CreateTemp(t.TempDir(), "received-")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			finished := make(chan error, 1)
			go func() { finished <- b.receive(context.Background(), 0, file) }()
			request := waitClipboardFileRequest(t, conn)
			size := make([]byte, 8)
			le.PutUint64(size, tc.size)
			if err := c.handleClipboard(clipPDU(clipFileContentsResponse, clipOK, join(request[:4], size))); err != nil {
				t.Fatal(err)
			}
			if tc.ok {
				request = waitClipboardFileRequest(t, conn)
				if le.Uint32(request[8:]) != clipFileRange || le.Uint32(request[20:]) > clipboardReceiveChunk {
					t.Fatal("large file did not use bounded range requests")
				}
				c.cleanupClipboardFiles() // Do not transfer gigabytes for this size-negotiation test.
			}
			select {
			case err := <-finished:
				if err == nil {
					t.Fatal("rejected or cancelled transfer succeeded")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("size rejection or cancellation stalled")
			}
		})
	}
}
