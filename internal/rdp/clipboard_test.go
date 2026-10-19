package rdp

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"anykvm/internal/capture"
	"anykvm/internal/clipboard"
)

type richImageMemoryClipboard struct {
	memoryClipboard
	html, rtf, image []byte
}

// imageFileReceiver advertises deferred-file support solely to verify that a
// concurrent browser image offer is selected before the virtual-file offer.
type imageFileReceiver struct{ *richImageMemoryClipboard }

func (imageFileReceiver) WriteDeferredFiles(context.Context, uint64, []capture.ClipboardFile,
	func(context.Context, int, *os.File) error) (uint64, func(), error) {
	return 0, nil, fmt.Errorf("unexpected virtual-file request")
}

func (b *richImageMemoryClipboard) ReadRichText() ([]byte, []byte, uint64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.html...), append([]byte(nil), b.rtf...), b.revision, nil
}

func (b *richImageMemoryClipboard) WriteRichText(html, rtf []byte) (uint64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.html, b.rtf, b.image, b.available = append([]byte(nil), html...), append([]byte(nil), rtf...), nil, false
	b.revision++
	return b.revision, nil
}

func (b *richImageMemoryClipboard) ReadImage() ([]byte, uint64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.image...), b.revision, nil
}

func (b *richImageMemoryClipboard) WriteImage(data []byte) (uint64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.image, b.html, b.rtf, b.available = append([]byte(nil), data...), nil, nil, false
	b.revision++
	return b.revision, nil
}

func testClipboardPNG(t *testing.T) []byte {
	t.Helper()
	image := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	image.SetNRGBA(0, 0, color.NRGBA{R: 0x11, G: 0x22, B: 0x33, A: 0x44})
	image.SetNRGBA(1, 0, color.NRGBA{R: 0xaa, G: 0xbb, B: 0xcc, A: 0xff})
	var out bytes.Buffer
	if err := png.Encode(&out, image); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func testClipboardJPEG(t *testing.T) []byte {
	t.Helper()
	image := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	image.SetNRGBA(0, 0, color.NRGBA{R: 0x11, G: 0x22, B: 0x33, A: 0xff})
	image.SetNRGBA(1, 0, color.NRGBA{R: 0xaa, G: 0xbb, B: 0xcc, A: 0xff})
	var out bytes.Buffer
	if err := jpeg.Encode(&out, image, nil); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

type memoryClipboard struct {
	mu        sync.Mutex
	text      string
	available bool
	revision  uint64
	writes    int
}

func (b *memoryClipboard) ReadText() (string, bool, uint64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text, b.available, b.revision, nil
}

func (b *memoryClipboard) WriteText(text string) (uint64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.text, b.available = text, true
	b.revision++
	b.writes++
	return b.revision, nil
}

func clipboardTestClient(t *testing.T) (*client, *memoryClipboard, *pointerRecordingConn) {
	t.Helper()
	b := &memoryClipboard{text: "Mac 原文", available: true, revision: 1}
	conn := &pointerRecordingConn{packets: make(chan []byte, 4096)}
	c := channelTestClient()
	c.s.cfg.Clipboard, c.conn, c.data.clipboard = b, conn, 1005
	c.clipboard.started = true
	t.Cleanup(func() {
		select {
		case <-c.done:
		default:
			close(c.done)
		}
		c.s.wg.Wait()
	})
	return c, b, conn
}

func clipboardPackets(t *testing.T, conn *pointerRecordingConn) [][]byte {
	t.Helper()
	var messages [][]byte
	var buf []byte
	total := 0
	for len(conn.packets) > 0 {
		packet := <-conn.packets
		p, fast, err := readPacket(bytes.NewReader(packet))
		if err != nil || fast || be.Uint16(p[6:]) != 1005 {
			t.Fatal("clipboard sent on incorrect transport/channel")
		}
		n, p, err := takePer(p[9:])
		if err != nil || n != len(p) || len(p) < 8 || len(p)-8 > vcChunk {
			t.Fatal("invalid static clipboard fragment")
		}
		flags := le.Uint32(p[4:])
		if flags&1 != 0 {
			if total != 0 {
				t.Fatal("overlapping output fragments")
			}
			total, buf = int(le.Uint32(p)), nil
		}
		if total != int(le.Uint32(p)) {
			t.Fatal("fragment total changed")
		}
		buf = append(buf, p[8:]...)
		if flags&2 != 0 {
			if len(buf) != total {
				t.Fatal("incomplete output message")
			}
			messages = append(messages, buf)
			total, buf = 0, nil
		}
	}
	if total != 0 {
		t.Fatal("missing final output fragment")
	}
	return messages
}

func clipboardOffer(long bool, text bool) []byte {
	if !text {
		return nil
	}
	if long {
		return join(u32(clipUnicodeText), u16(0))
	}
	return join(u32(clipUnicodeText), make([]byte, 32))
}

func TestClipboardDisabled(t *testing.T) {
	for _, supplied := range []bool{false, true} {
		cfg := Config{
			Addr: "127.0.0.1:3389", Username: "test", Password: "test",
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{{}}},
		}
		if supplied {
			cfg.Clipboard = &memoryClipboard{}
		}
		enabled, err := NewServer(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if supplied && enabled.cfg.Clipboard != cfg.Clipboard {
			t.Fatal("default configuration must retain the supplied clipboard backend")
		}
		if !supplied && (enabled.cfg.Clipboard != nil) != (capture.NewClipboard() != nil) {
			t.Fatal("default configuration must select the platform clipboard backend")
		}
		cfg.DisableClipboard = true
		disabled, err := NewServer(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if disabled.cfg.Clipboard != nil {
			t.Fatal("disabled clipboard must not retain or select a backend")
		}
		c, _, conn := clipboardTestClient(t)
		c.s = disabled
		c.clipboard.started = false
		if err := c.startClipboard(); err != nil {
			t.Fatal(err)
		}
		if c.clipboard.started || len(conn.packets) != 0 {
			t.Fatal("disabled clipboard must not initialize the channel")
		}
	}
}

func TestClipboardTextPolicyConfiguration(t *testing.T) {
	for _, direction := range []clipboard.Direction{-1, 0, 1, 2, 3, 4} {
		t.Run(fmt.Sprint(direction), func(t *testing.T) {
			options := clipboard.Options{Text: direction, Files: 3, Images: 3, RichText: 3}
			cfg := Config{
				Addr: "127.0.0.1:3389", Username: "test", Password: "test",
				TLSConfig: &tls.Config{Certificates: []tls.Certificate{{}}},
				Clipboard: &memoryClipboard{}, ClipboardOptions: &options,
			}
			s, err := NewServer(cfg)
			if direction < 0 || direction > 3 {
				if err == nil {
					t.Fatal("accepted an invalid text direction")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (s.cfg.Clipboard == nil) != (direction == 0) {
				t.Fatal("text direction 0 must disable a backend without file capability")
			}
			options.Text = 0
			if s.cfg.ClipboardOptions.Text != direction {
				t.Fatal("caller mutation changed the running server's clipboard policy")
			}
			if direction == 0 {
				c, _, conn := clipboardTestClient(t)
				c.s, c.clipboard.started = s, false
				if err := c.startClipboard(); err != nil || c.clipboard.started || len(conn.packets) != 0 {
					t.Fatal("text direction 0 initialized the clipboard channel", err)
				}
			}
		})
	}
}

func TestClipboardTextDirections(t *testing.T) {
	for _, direction := range []clipboard.Direction{0, 1, 2, 3} {
		t.Run(fmt.Sprint(direction), func(t *testing.T) {
			c, b, conn := clipboardTestClient(t)
			c.s.cfg.ClipboardOptions = &clipboard.Options{Text: direction}
			receive := direction == 1 || direction == 3
			send := direction == 2 || direction == 3
			if err := c.handleClipboard(clipPDU(clipFormatList, 0, clipboardOffer(false, true))); err != nil {
				t.Fatal(err)
			}
			p := clipboardPackets(t, conn)
			want := 1
			if direction == 0 {
				want = 0
			} else if receive {
				want = 2
			}
			if len(p) != want || (receive && le.Uint16(p[1]) != clipDataRequest) {
				t.Fatal("client offer requested text in a blocked direction")
			}
			if !receive {
				// Even a response matching an outstanding request must not bypass policy.
				c.clipboard.requestPending = true
				c.clipboard.requestGeneration = c.clipboard.generation
				c.clipboard.requestRevision = 1
			}
			encoded, _ := clipboardEncodeText("client copy")
			if err := c.handleClipboard(clipPDU(clipDataResponse, clipOK, encoded)); err != nil {
				t.Fatal(err)
			}
			got, _, _, _ := b.ReadText()
			if (receive && (got != "client copy" || b.writes != 1)) || (!receive && (got != "Mac 原文" || b.writes != 0)) {
				t.Fatal("incoming text did not respect direction policy")
			}
			if !receive {
				if err := c.requestClipboardData(); err != nil || len(conn.packets) != 0 {
					t.Fatal("blocked incoming direction issued a text request", err)
				}
			}
			_, _ = b.WriteText("host copy")
			if err := c.pollClipboard(); err != nil {
				t.Fatal(err)
			}
			p = clipboardPackets(t, conn)
			if send {
				if len(p) != 1 || !bytes.Equal(p[0], clipPDU(clipFormatList, 0, clipboardOffer(false, true))) {
					t.Fatal("allowed host direction did not advertise text")
				}
				if err := c.handleClipboard(clipPDU(clipFormatListResponse, clipOK, nil)); err != nil {
					t.Fatal(err)
				}
			} else {
				if len(p) != 0 {
					t.Fatal("blocked host direction advertised clipboard data")
				}
				// A request must fail even if the peer can bypass advertisement checks.
				c.clipboard.localText = "host copy"
				c.clipboard.localAvailable, c.clipboard.listAccepted = true, true
			}
			if direction == 2 {
				// A denied incoming offer must not invalidate the allowed host snapshot.
				if err := c.handleClipboard(clipPDU(clipFormatList, 0, clipboardOffer(false, true))); err != nil {
					t.Fatal(err)
				}
				clipboardPackets(t, conn)
			}
			if err := c.handleClipboard(clipPDU(clipDataRequest, 0, u32(clipUnicodeText))); err != nil {
				t.Fatal(err)
			}
			p = clipboardPackets(t, conn)
			if direction == 0 {
				if len(p) != 0 {
					t.Fatal("disabled text direction emitted a response")
				}
			} else if send {
				if len(p) != 1 || le.Uint16(p[0][2:]) != clipOK {
					t.Fatal("allowed host text request failed")
				}
				text, err := clipboardDecodeText(p[0][8:])
				if err != nil || text != "host copy" {
					t.Fatal("incorrect host clipboard response", err)
				}
			} else if len(p) != 1 || !bytes.Equal(p[0], clipPDU(clipDataResponse, clipFail, nil)) {
				t.Fatal("blocked host direction disclosed clipboard text")
			}
		})
	}
}

func TestClipboardChannelNegotiation(t *testing.T) {
	core := make([]byte, 142)
	le.PutUint16(core[4:], 1920)
	le.PutUint16(core[6:], 1080)
	le.PutUint16(core[140:], 0x100)
	channels := join(u32(2), []byte("cliprdr\x00"), u32(0), []byte("drdynvc\x00"), u32(0))
	blocks := join(capability(0xc001, core), capability(0xc003, channels))
	gcc := join([]byte("Duca"), perLength(len(blocks)), blocks)
	value := join(berValue([]byte{4}, nil), berValue([]byte{4}, nil), berValue([]byte{1}, nil),
		berValue([]byte{0x30}, nil), berValue([]byte{0x30}, nil), berValue([]byte{0x30}, nil), berValue([]byte{4}, gcc))
	data, err := parseConnect(join([]byte{2, 0xf0, 0x80}, berValue([]byte{0x7f, 0x65}, value)))
	if err != nil || data.clipboard != 1004 || data.dvc != 1005 {
		t.Fatalf("channel allocation: %+v %v", data, err)
	}
	c, _, conn := clipboardTestClient(t)
	c.clipboard.started = false
	if err := c.startClipboard(); err != nil {
		t.Fatal(err)
	}
	p := clipboardPackets(t, conn)
	if len(p) != 2 || le.Uint16(p[0]) != clipCapabilities || le.Uint16(p[1]) != clipMonitorReady {
		t.Fatalf("initialization sequence: %x", p)
	}
	long, err := clipboardLongNames(p[0][8:])
	if err != nil || !long {
		t.Fatal("server did not advertise long format names")
	}
	if err := c.startClipboard(); err != nil || len(conn.packets) != 0 {
		t.Fatal("repeated initialization")
	}
	if err := c.pollClipboard(); err != nil || len(conn.packets) != 0 {
		t.Fatal("server overwrote initial client offer")
	}
}

func TestClipboardBidirectionalUnicodeAndNoEcho(t *testing.T) {
	for _, long := range []bool{false, true} {
		t.Run(map[bool]string{false: "short", true: "long"}[long], func(t *testing.T) {
			c, b, conn := clipboardTestClient(t)
			if long {
				if err := c.handleClipboard(clipPDU(clipCapabilities, 0, join(u16(1), u16(0), u16(1), u16(12), u32(2), u32(2)))); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.handleClipboard(clipPDU(clipFormatList, 0, clipboardOffer(long, true))); err != nil {
				t.Fatal(err)
			}
			p := clipboardPackets(t, conn)
			if len(p) != 2 || le.Uint16(p[0]) != clipFormatListResponse || le.Uint16(p[0][2:]) != clipOK ||
				le.Uint16(p[1]) != clipDataRequest || le.Uint32(p[1][8:]) != clipUnicodeText {
				t.Fatalf("client offer: %x", p)
			}
			text := "Windows 中文😀\n第二行"
			encoded, _ := clipboardEncodeText(text)
			if err := c.handleClipboard(clipPDU(clipDataResponse, clipOK, encoded)); err != nil {
				t.Fatal(err)
			}
			got, available, _, _ := b.ReadText()
			if got != text || !available || b.writes != 1 {
				t.Fatalf("host clipboard=%q", got)
			}
			if err := c.pollClipboard(); err != nil || len(conn.packets) != 0 {
				t.Fatal("remote copy echoed back")
			}
			_, _ = b.WriteText("Mac 日本語🙂\n新行")
			if err := c.pollClipboard(); err != nil {
				t.Fatal(err)
			}
			p = clipboardPackets(t, conn)
			if len(p) != 1 || !bytes.Equal(p[0], clipPDU(clipFormatList, 0, clipboardOffer(long, true))) {
				t.Fatalf("host offer: %x", p)
			}
			if err := c.handleClipboard(clipPDU(clipFormatListResponse, clipOK, nil)); err != nil {
				t.Fatal(err)
			}
			if err := c.handleClipboard(clipPDU(clipDataRequest, 0, u32(clipUnicodeText))); err != nil {
				t.Fatal(err)
			}
			p = clipboardPackets(t, conn)
			if len(p) != 1 || le.Uint16(p[0][2:]) != clipOK {
				t.Fatal("missing host text response")
			}
			got, err := clipboardDecodeText(p[0][8:])
			if err != nil || got != "Mac 日本語🙂\n新行" {
				t.Fatalf("Windows text=%q %v", got, err)
			}
		})
	}
}

func TestClipboardRichTextAndImage(t *testing.T) {
	pngData := testClipboardPNG(t)
	b := &richImageMemoryClipboard{memoryClipboard: memoryClipboard{revision: 1}, html: []byte("<b>Mac</b>"), rtf: []byte("{\\rtf1 Mac}"), image: pngData}
	c, _, conn := clipboardTestClient(t)
	c.s.cfg.Clipboard = b
	c.s.cfg.ClipboardOptions = &clipboard.Options{RichText: clipboard.Bidirectional, Images: clipboard.Bidirectional}
	c.clipboard.peerReady, c.clipboard.longNames = true, true
	if err := c.pollClipboard(); err != nil {
		t.Fatal(err)
	}
	p := clipboardPackets(t, conn)
	wantOffer := clipboardFormatList(true, []clipboardFormat{{clipDIBV5, ""}, {clipHTML, clipHTMLName}, {clipRTF, clipRTFName}})
	if len(p) != 1 || !bytes.Equal(p[0], clipPDU(clipFormatList, 0, wantOffer)) {
		t.Fatalf("rich/image host offer: %x", p)
	}
	if err := c.handleClipboard(clipPDU(clipFormatListResponse, clipOK, nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.handleClipboard(clipPDU(clipDataRequest, 0, u32(clipHTML))); err != nil {
		t.Fatal(err)
	}
	p = clipboardPackets(t, conn)
	if len(p) != 1 || le.Uint16(p[0][2:]) != clipOK {
		t.Fatal("missing HTML response")
	}
	got, err := clipboardDecodeHTML(p[0][8:])
	if err != nil || string(got) != "<b>Mac</b>" {
		t.Fatalf("HTML response=%q %v", got, err)
	}
	if err := c.handleClipboard(clipPDU(clipDataRequest, 0, u32(clipDIBV5))); err != nil {
		t.Fatal(err)
	}
	p = clipboardPackets(t, conn)
	if len(p) != 1 || le.Uint16(p[0][2:]) != clipOK {
		t.Fatal("missing DIBV5 response")
	}
	gotPNG, err := dibToPNG(p[0][8:])
	if err != nil || !bytes.Equal(gotPNG, pngData) {
		t.Fatalf("DIBV5 image did not round trip: %v", err)
	}

	remoteHTML, remoteImage := uint32(0xc111), uint32(clipDIBV5)
	offer := clipboardFormatList(true, []clipboardFormat{{remoteHTML, clipHTMLName}, {remoteImage, ""}})
	if err := c.handleClipboard(clipPDU(clipFormatList, 0, offer)); err != nil {
		t.Fatal(err)
	}
	p = clipboardPackets(t, conn)
	if len(p) != 2 || le.Uint16(p[1]) != clipDataRequest || le.Uint32(p[1][8:]) != clipDIBV5 {
		t.Fatalf("image was not requested first: %x", p)
	}
	dib, err := pngToDIBV5(pngData)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.handleClipboard(clipPDU(clipDataResponse, clipOK, dib)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b.image, pngData) {
		t.Fatal("remote image was not written")
	}

	// A fresh offer may select HTML after the image snapshot has replaced the host copy.
	if err := c.handleClipboard(clipPDU(clipFormatList, 0, clipboardFormatList(true, []clipboardFormat{{remoteHTML, clipHTMLName}}))); err != nil {
		t.Fatal(err)
	}
	p = clipboardPackets(t, conn)
	if len(p) != 2 || le.Uint32(p[1][8:]) != remoteHTML {
		t.Fatalf("HTML was not requested: %x", p)
	}
	if err := c.handleClipboard(clipPDU(clipDataResponse, clipOK, clipboardEncodeHTML([]byte("<i>Windows</i>")))); err != nil {
		t.Fatal(err)
	}
	if string(b.html) != "<i>Windows</i>" || len(b.rtf) != 0 {
		t.Fatalf("remote HTML=%q rtf=%q", b.html, b.rtf)
	}
}

func TestClipboardImageDIBForms(t *testing.T) {
	pngData := testClipboardPNG(t)
	dib, err := pngToDIBV5(pngData)
	if err != nil {
		t.Fatal(err)
	}
	got, err := dibToPNG(dib)
	if err != nil || !bytes.Equal(got, pngData) {
		t.Fatalf("DIBV5 conversion: %v", err)
	}
	// CF_DIB clients also commonly send bottom-up, 24-bit BI_RGB data.
	dib24 := make([]byte, 44)
	le.PutUint32(dib24, 40)
	le.PutUint32(dib24[4:], 1)
	le.PutUint32(dib24[8:], 1)
	le.PutUint16(dib24[12:], 1)
	le.PutUint16(dib24[14:], 24)
	dib24[40], dib24[41], dib24[42] = 0x33, 0x22, 0x11
	got, err = dibToPNG(dib24)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(got))
	if err != nil || color.NRGBAModel.Convert(decoded.At(0, 0)).(color.NRGBA) != (color.NRGBA{R: 0x11, G: 0x22, B: 0x33, A: 0xff}) {
		t.Fatalf("24-bit DIB conversion: %v", err)
	}
}

func TestClipboardReceiveRegisteredPNG(t *testing.T) {
	pngData := testClipboardPNG(t)
	b := &richImageMemoryClipboard{memoryClipboard: memoryClipboard{revision: 1}}
	c, _, conn := clipboardTestClient(t)
	c.s.cfg.Clipboard = b
	c.s.cfg.ClipboardOptions = &clipboard.Options{Images: clipboard.ClientToServer}
	c.clipboard.longNames = true
	format := uint32(0xc147)
	offer := clipboardFormatList(true, []clipboardFormat{{clipDIBV5, ""}, {format, clipPNGName}})
	if parsed, err := parseClipboardFormats(offer, true, false); err != nil || parsed.image != format || parsed.imageEncoding != clipPNGName {
		t.Fatalf("registered PNG parse=%+v err=%v", parsed, err)
	}
	if err := c.handleClipboard(clipPDU(clipFormatList, 0, offer)); err != nil {
		t.Fatal(err)
	}
	p := clipboardPackets(t, conn)
	if len(p) != 2 || le.Uint16(p[1]) != clipDataRequest || le.Uint32(p[1][8:]) != format {
		t.Fatalf("registered PNG was not preferred: %x", p)
	}
	if err := c.handleClipboard(clipPDU(clipDataResponse, clipOK, pngData)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b.image, pngData) {
		t.Fatal("registered PNG was not published to macOS clipboard backend")
	}
}

func TestClipboardReceiveRegisteredJPEG(t *testing.T) {
	jpegData := testClipboardJPEG(t)
	b := &richImageMemoryClipboard{memoryClipboard: memoryClipboard{revision: 1}}
	c, _, conn := clipboardTestClient(t)
	c.s.cfg.Clipboard = b
	c.s.cfg.ClipboardOptions = &clipboard.Options{Images: clipboard.ClientToServer}
	c.clipboard.longNames = true
	format := uint32(0xc148)
	if err := c.handleClipboard(clipPDU(clipFormatList, 0, clipboardFormatList(true, []clipboardFormat{{format, clipJFIFName}}))); err != nil {
		t.Fatal(err)
	}
	p := clipboardPackets(t, conn)
	if len(p) != 2 || le.Uint32(p[1][8:]) != format {
		t.Fatalf("registered JFIF was not requested: %x", p)
	}
	if err := c.handleClipboard(clipPDU(clipDataResponse, clipOK, jpegData)); err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(b.image))
	if err != nil || decoded.Bounds().Dx() != 2 || decoded.Bounds().Dy() != 1 {
		t.Fatalf("registered JFIF was not converted to PNG: %v", err)
	}
}

func TestClipboardImagePreferredOverVirtualFile(t *testing.T) {
	pngData := testClipboardPNG(t)
	b := &richImageMemoryClipboard{memoryClipboard: memoryClipboard{revision: 1}}
	c, _, conn := clipboardTestClient(t)
	c.s.cfg.Clipboard = imageFileReceiver{b}
	c.s.cfg.ClipboardOptions = &clipboard.Options{Images: clipboard.ClientToServer, Files: clipboard.ClientToServer}
	c.clipboard.longNames, c.clipboard.peerFiles = true, true
	offer := clipboardFormatList(true, []clipboardFormat{{clipDIBV5, ""}, {0xc149, clipFileGroupName}})
	if err := c.handleClipboard(clipPDU(clipFormatList, 0, offer)); err != nil {
		t.Fatal(err)
	}
	p := clipboardPackets(t, conn)
	if len(p) != 2 || le.Uint16(p[1]) != clipDataRequest || le.Uint32(p[1][8:]) != clipDIBV5 {
		t.Fatalf("image was not preferred over a virtual file: %x", p)
	}
	dib, err := pngToDIBV5(pngData)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.handleClipboard(clipPDU(clipDataResponse, clipOK, dib)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b.image, pngData) {
		t.Fatal("preferred image was not published")
	}
}

func TestClipboardDelayedResponsesDoNotOverwriteNewCopies(t *testing.T) {
	for _, mode := range []string{"host-copy", "new-remote-offer", "host-after-remote-offer", "remote-without-text", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			c, b, conn := clipboardTestClient(t)
			if err := c.handleClipboard(clipPDU(clipFormatList, 0, clipboardOffer(false, true))); err != nil {
				t.Fatal(err)
			}
			clipboardPackets(t, conn)
			switch mode {
			case "host-copy":
				_, _ = b.WriteText("new host copy")
			case "new-remote-offer", "host-after-remote-offer", "remote-without-text":
				if err := c.handleClipboard(clipPDU(clipFormatList, 0, clipboardOffer(false, mode != "remote-without-text"))); err != nil {
					t.Fatal(err)
				}
				p := clipboardPackets(t, conn)
				if len(p) != 1 {
					t.Fatal("overlapping data requests")
				}
				if mode == "host-after-remote-offer" {
					_, _ = b.WriteText("host copy after queued offer")
				}
			case "disconnect":
				close(c.done)
			}
			text, _ := clipboardEncodeText("stale remote copy")
			if err := c.handleClipboard(clipPDU(clipDataResponse, clipOK, text)); err != nil {
				t.Fatal(err)
			}
			got, _, _, _ := b.ReadText()
			if got == "stale remote copy" {
				t.Fatal("stale response overwrote clipboard")
			}
			if mode == "host-after-remote-offer" && (got != "host copy after queued offer" || len(conn.packets) != 0) {
				t.Fatal("queued remote offer superseded a newer host copy")
			}
			if mode == "new-remote-offer" {
				p := clipboardPackets(t, conn)
				if len(p) != 1 || le.Uint16(p[0]) != clipDataRequest {
					t.Fatal("latest remote offer was lost")
				}
				text, _ = clipboardEncodeText("latest remote copy")
				if err := c.handleClipboard(clipPDU(clipDataResponse, clipOK, text)); err != nil {
					t.Fatal(err)
				}
				got, _, _, _ = b.ReadText()
				if got != "latest remote copy" {
					t.Fatal("latest remote copy not applied")
				}
			}
		})
	}
}

func TestClipboardAcknowledgementsSnapshotsAndUnsupportedFormats(t *testing.T) {
	c, b, conn := clipboardTestClient(t)
	c.clipboard.peerReady = true
	if err := c.pollClipboard(); err != nil {
		t.Fatal(err)
	}
	clipboardPackets(t, conn)
	_, _ = b.WriteText("newer unadvertised copy")
	if err := c.pollClipboard(); err != nil || len(conn.packets) != 0 {
		t.Fatal("offers overlapped before acknowledgement")
	}
	if err := c.handleClipboard(clipPDU(clipFormatListResponse, clipOK, nil)); err != nil {
		t.Fatal(err)
	}
	if err := c.handleClipboard(clipPDU(clipDataRequest, 0, u32(clipUnicodeText))); err != nil {
		t.Fatal(err)
	}
	p := clipboardPackets(t, conn)
	text, _ := clipboardDecodeText(p[0][8:])
	if text != "Mac 原文" {
		t.Fatal("response did not use advertised snapshot")
	}
	if err := c.pollClipboard(); err != nil {
		t.Fatal(err)
	}
	clipboardPackets(t, conn)
	if err := c.handleClipboard(clipPDU(clipFormatListResponse, clipFail, nil)); err != nil {
		t.Fatal(err)
	}
	for _, format := range []uint32{clipUnicodeText, 15, 0xc001} {
		if err := c.handleClipboard(clipPDU(clipDataRequest, 0, u32(format))); err != nil {
			t.Fatal(err)
		}
		p = clipboardPackets(t, conn)
		if len(p) != 1 || !bytes.Equal(p[0], clipPDU(clipDataResponse, clipFail, nil)) {
			t.Fatal("unsupported/rejected format was served")
		}
	}
}

func TestClipboardFragmentationAndIsolation(t *testing.T) {
	c, b, conn := clipboardTestClient(t)
	if err := c.handleClipboard(clipPDU(clipFormatList, 0, clipboardOffer(false, true))); err != nil {
		t.Fatal(err)
	}
	clipboardPackets(t, conn)
	text := strings.Repeat("中😀\n", 2000)
	encoded, _ := clipboardEncodeText(text)
	p := clipPDU(clipDataResponse, clipOK, encoded)
	if err := c.sendStaticVC(1005, p); err != nil {
		t.Fatal(err)
	}
	var packets [][]byte
	for len(conn.packets) > 0 {
		packets = append(packets, <-conn.packets)
	}
	if len(packets) < 3 {
		t.Fatal("large text was not fragmented")
	}
	for i, packet := range packets {
		payload, _, err := readPacket(bytes.NewReader(packet))
		if err != nil {
			t.Fatal(err)
		}
		_, payload, _ = takePer(payload[9:])
		if err := c.handleClipboardVC(payload); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			// A separate static-channel message may arrive between fragments.
			c.vcTotal, c.vcBuf = 11, []byte{1, 2, 3}
		}
	}
	got, _, _, _ := b.ReadText()
	if got != text || c.vcTotal != 11 || !bytes.Equal(c.vcBuf, []byte{1, 2, 3}) {
		t.Fatal("fragment reassembly mixed channels or lost Unicode")
	}
}

func TestClipboardMalformedAndOversizedInput(t *testing.T) {
	for _, p := range [][]byte{
		nil, make([]byte, 7), join(u32(maxClipboardMessage+1), u32(3)),
		join(u32(8), u32(2), make([]byte, 8)),
		join(u32(8), u32(3), make([]byte, 7)),
		join(u32(8), u32(0x103), make([]byte, 8)),
		join(u32(8), u32(3), clipPDU(clipDataRequest, 0, nil)),
	} {
		c, _, _ := clipboardTestClient(t)
		if err := c.handleClipboardVC(p); err == nil {
			t.Fatalf("accepted malformed channel packet %x", p)
		}
	}
	for _, p := range [][]byte{u16(0), join(u16(1), u16(0)), join(u16(1), u16(0), u16(1), u16(8), u32(2))} {
		if _, err := clipboardLongNames(p); err == nil {
			t.Fatal("accepted malformed capabilities")
		}
	}
	for _, p := range [][]byte{u32(13), join(u32(13), u16(42)), make([]byte, 35)} {
		if _, err := clipboardHasUnicode(p, false); err == nil {
			t.Fatal("accepted malformed short format")
		}
	}
	if _, err := clipboardHasUnicode(join(u32(13), u16(42)), true); err == nil {
		t.Fatal("accepted unterminated long name")
	}
	for _, p := range [][]byte{nil, {1}, u16(42), join(u16(0xd800), u16(0)), join(u16(0xdc00), u16(0))} {
		if _, err := clipboardDecodeText(p); err == nil {
			t.Fatalf("accepted invalid Unicode %x", p)
		}
	}
	for _, text := range []string{"bad\x00text", string([]byte{0xff}), strings.Repeat("a", maxClipboardMessage)} {
		if _, err := clipboardEncodeText(text); err == nil {
			t.Fatal("accepted invalid/oversized host text")
		}
	}
	c, b, conn := clipboardTestClient(t)
	if err := c.handleClipboard(clipPDU(clipFormatList, 0, clipboardOffer(false, true))); err != nil {
		t.Fatal(err)
	}
	clipboardPackets(t, conn)
	if err := c.handleClipboard(clipPDU(clipDataResponse, clipOK, []byte{1})); err != nil {
		t.Fatal(err)
	}
	if b.writes != 0 {
		t.Fatal("invalid data changed the host clipboard")
	}
}

func TestClipboardEmptyTextAndPollingCleanup(t *testing.T) {
	p, err := clipboardEncodeText("")
	if err != nil || !bytes.Equal(p, []byte{0, 0}) {
		t.Fatal("empty text encoding")
	}
	text, err := clipboardDecodeText(p)
	if err != nil || text != "" {
		t.Fatal("empty text decoding")
	}
	c, _, _ := clipboardTestClient(t)
	c.clipboard.started = false
	if err := c.startClipboard(); err != nil {
		t.Fatal(err)
	}
	// The production worker must leave promptly when the connection closes.
	finished := make(chan struct{})
	close(c.done)
	go func() { c.s.wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("clipboard worker leaked")
	}
}

func TestClipboardIgnoredChannelFlags(t *testing.T) {
	for _, flags := range []uint32{0x23, 0x43, 0x83, 0xf3} {
		c, _, _ := clipboardTestClient(t)
		if err := c.handleClipboardVC(join(u32(8), u32(flags), clipPDU(clipFormatList, 0, nil))); err != nil {
			t.Fatalf("legal ignored flags %#x: %v", flags, err)
		}
	}
}
