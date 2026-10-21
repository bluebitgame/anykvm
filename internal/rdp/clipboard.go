package rdp

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"anykvm/internal/capture"
	"anykvm/internal/clipboard"
	"anykvm/internal/logging"
)

const (
	clipMonitorReady       = 1
	clipFormatList         = 2
	clipFormatListResponse = 3
	clipDataRequest        = 4
	clipDataResponse       = 5
	clipCapabilities       = 7
	clipOK                 = 1
	clipFail               = 2
	clipDIB                = 8  // CF_DIB
	clipUnicodeText        = 13 // CF_UNICODETEXT
	clipDIBV5              = 17 // CF_DIBV5
	clipHTML               = 0xc002
	clipRTF                = 0xc003
	clipHTMLName           = "HTML Format"
	clipRTFName            = "Rich Text Format"
	clipPNGName            = "PNG"
	clipJPEGName           = "JPEG"
	clipJFIFName           = "JFIF"
	clipGIFName            = "GIF"
	maxClipboardMessage    = 16 << 20
)

// The protocol reader owns fragment assembly. All other clipboard state is
// shared with the polling worker and protected by mu.
type clipboardState struct {
	mu                                        sync.Mutex
	buf                                       []byte
	total                                     int
	started, peerReady, longNames             bool
	haveRevision                              bool
	revision                                  uint64
	localText                                 string
	localHTML, localRTF, localImage           []byte
	localAvailable, listPending, listAccepted bool
	remoteText                                bool
	remoteHTML, remoteRTF, remoteImage        uint32
	remoteImageEncoding                       string
	offerRevision                             uint64
	offerReadable                             bool
	generation, requestGeneration             uint64
	requestPending                            bool
	requestRevision                           uint64
	requestTime                               time.Time
	backendFailed                             bool
	peerFiles                                 bool
	localFiles                                *clipboardFiles
	remoteFileFormat, requestFormat           uint32
	remoteFiles                               *remoteClipboardFiles
	nextFileStream                            uint32
	fileReads                                 map[uint32]*clipboardFileRead
	filePaddingLogged                         bool
}

func clipPDU(kind, flags int, data []byte) []byte {
	return join(u16(kind), u16(flags), u32(uint32(len(data))), data)
}

func (c *client) sendClipboard(kind, flags int, data []byte) error {
	return c.sendStaticVC(c.data.clipboard, clipPDU(kind, flags, data))
}

func (c *client) clipboardTextAllowed(flow clipboard.Direction) bool {
	if c.s.cfg.DisableClipboard {
		return false
	}
	options := c.s.cfg.ClipboardOptions
	if options == nil {
		return clipboard.Bidirectional.Allows(flow)
	}
	return options.Text.Allows(flow)
}

func (c *client) clipboardRichTextAllowed(flow clipboard.Direction) bool {
	if c.s.cfg.DisableClipboard {
		return false
	}
	options := c.s.cfg.ClipboardOptions
	return options != nil && options.RichText.Allows(flow)
}

func (c *client) clipboardImagesAllowed(flow clipboard.Direction) bool {
	if c.s.cfg.DisableClipboard {
		return false
	}
	options := c.s.cfg.ClipboardOptions
	return options != nil && options.Images.Allows(flow)
}

func (c *client) clipboardSendRichTextAllowed() bool {
	_, supported := c.s.cfg.Clipboard.(capture.RichTextClipboard)
	return supported && c.clipboardRichTextAllowed(clipboard.ServerToClient)
}

func (c *client) clipboardReceiveRichTextAllowed() bool {
	_, supported := c.s.cfg.Clipboard.(capture.RichTextClipboard)
	return supported && c.clipboardRichTextAllowed(clipboard.ClientToServer)
}

func (c *client) clipboardSendImagesAllowed() bool {
	_, supported := c.s.cfg.Clipboard.(capture.ImageClipboard)
	return supported && c.clipboardImagesAllowed(clipboard.ServerToClient)
}

func (c *client) clipboardReceiveImagesAllowed() bool {
	_, supported := c.s.cfg.Clipboard.(capture.ImageClipboard)
	return supported && c.clipboardImagesAllowed(clipboard.ClientToServer)
}

func (c *client) clipboardRichTextAllowedAny() bool {
	return c.clipboardSendRichTextAllowed() || c.clipboardReceiveRichTextAllowed()
}

func (c *client) clipboardImagesAllowedAny() bool {
	return c.clipboardSendImagesAllowed() || c.clipboardReceiveImagesAllowed()
}

func (c *client) startClipboard() error {
	if c.data.clipboard == 0 || c.s.cfg.Clipboard == nil ||
		(!c.clipboardTextAllowed(clipboard.ClientToServer) && !c.clipboardTextAllowed(clipboard.ServerToClient) && !c.clipboardFilesAllowed() && !c.clipboardRichTextAllowedAny() && !c.clipboardImagesAllowedAny()) {
		return nil
	}
	s := &c.clipboard
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil
	}
	s.started = true
	capabilities := uint32(2) // Long names; no clipboard locking.
	if c.clipboardFilesAllowed() {
		capabilities |= clipStreamFiles | clipNoFilePaths
	}
	if err := c.sendClipboard(clipCapabilities, 0, join(u16(1), u16(0), u16(1), u16(12), u32(2), u32(capabilities))); err != nil {
		return err
	}
	if err := c.sendClipboard(clipMonitorReady, 0, nil); err != nil {
		return err
	}
	if !c.clipboardTextAllowed(clipboard.ServerToClient) && !c.clipboardFilesAllowed() && !c.clipboardSendRichTextAllowed() && !c.clipboardSendImagesAllowed() {
		return nil // Client-to-server text is driven by incoming channel messages.
	}
	c.s.wg.Add(1)
	go func() {
		defer c.s.wg.Done()
		c.clipboardLoop()
	}()
	return nil
}

func (c *client) clipboardLoop() {
	defer c.cleanupClipboardFiles()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			if err := c.pollClipboard(); err != nil {
				logging.Infof("rdp: clipboard channel: %v", err)
				c.close()
				return
			}
		}
	}
}

func (c *client) pollClipboard() error {
	if !c.clipboardTextAllowed(clipboard.ServerToClient) && !c.clipboardFilesAllowed() && !c.clipboardSendRichTextAllowed() && !c.clipboardSendImagesAllowed() {
		return nil
	}
	s := &c.clipboard
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-c.done:
		return nil
	default:
	}
	if !s.peerReady {
		return nil
	}
	var files *clipboardFiles
	var paths []string
	var fileAvailable bool
	var fileRevision uint64
	if c.clipboardSendFilesAllowed() && s.peerFiles {
		var err error
		paths, fileAvailable, fileRevision, err = c.s.cfg.Clipboard.(capture.FileClipboard).ReadFiles()
		if errors.Is(err, capture.ErrClipboardFilesPending) {
			return nil
		}
		c.clipboardBackendResult(err)
		if err != nil {
			return nil
		}
	}
	text, available, revision, err := c.s.cfg.Clipboard.ReadText()
	c.clipboardBackendResult(err)
	if err != nil {
		return nil // A temporarily unavailable pasteboard must not end the session.
	}
	if s.haveRevision && revision == s.revision {
		return nil
	}
	if s.remoteFiles != nil {
		c.retireRemoteClipboardFiles()
	}
	if s.listPending || (s.requestPending && time.Since(s.requestTime) < 5*time.Second) {
		return nil
	}
	var html, rtf, image []byte
	if c.clipboardSendRichTextAllowed() {
		var richRevision uint64
		html, rtf, richRevision, err = c.s.cfg.Clipboard.(capture.RichTextClipboard).ReadRichText()
		c.clipboardBackendResult(err)
		if err != nil || richRevision != revision {
			return nil
		}
	}
	if c.clipboardSendImagesAllowed() {
		var imageRevision uint64
		image, imageRevision, err = c.s.cfg.Clipboard.(capture.ImageClipboard).ReadImage()
		c.clipboardBackendResult(err)
		if err != nil || imageRevision != revision {
			return nil
		}
	}
	if !c.clipboardTextAllowed(clipboard.ServerToClient) && !c.clipboardSendFilesAllowed() && !c.clipboardSendRichTextAllowed() && !c.clipboardSendImagesAllowed() {
		s.revision, s.haveRevision = revision, true
		return nil
	}
	if c.clipboardSendFilesAllowed() && s.peerFiles && revision != fileRevision {
		return nil // Text and file views must refer to the same host copy.
	}
	if fileAvailable {
		files, err = snapshotClipboardFiles(paths)
		if err != nil {
			logging.Infof("rdp: clipboard file offer: %v", err)
		}
		available = false // Do not advertise a file URL as an alternative text copy.
	} else if !c.clipboardTextAllowed(clipboard.ServerToClient) {
		available = false
	}
	if available {
		if _, err := clipboardEncodeText(text); err != nil {
			available = false
		}
	}
	var formats []clipboardFormat
	flags := 0
	if files != nil {
		formats = []clipboardFormat{{clipFileGroupDescriptor, clipFileGroupName}}
		if !s.longNames {
			flags = 4 // CB_ASCII_NAMES fits FileGroupDescriptorW in the short field.
		}
	} else {
		if len(image) != 0 {
			formats = append(formats, clipboardFormat{clipDIBV5, ""})
		}
		if len(html) != 0 {
			formats = append(formats, clipboardFormat{clipHTML, clipHTMLName})
		}
		if len(rtf) != 0 {
			formats = append(formats, clipboardFormat{clipRTF, clipRTFName})
		}
		if available {
			formats = append(formats, clipboardFormat{clipUnicodeText, ""})
		}
	}
	s.localFiles.close()
	s.localFiles = files
	s.localText, s.localHTML, s.localRTF, s.localImage, s.localAvailable = text, html, rtf, image, available
	s.revision, s.haveRevision = revision, true
	s.listPending, s.listAccepted = true, false
	payload := clipboardFormatList(s.longNames, formats)
	if files != nil {
		// FileGroupDescriptorW uses an ASCII short name when CB_ASCII_NAMES
		// is set; keep the legacy short-form wire representation exact.
		payload = clipboardFileFormat(s.longNames)
	}
	return c.sendClipboard(clipFormatList, flags, payload)
}

func (c *client) handleClipboardVC(p []byte) error {
	s := &c.clipboard
	if len(p) < 8 {
		return fmt.Errorf("%w: clipboard channel header", errProtocol)
	}
	total, flags := int(le.Uint32(p)), le.Uint32(p[4:])
	p = p[8:]
	// MS-RDPBCGR 2.2.6.1.1 requires SUSPEND/RESUME and SHADOW_PERSISTENT
	// to be ignored on client-to-server traffic. They do not change assembly.
	if flags & ^uint32(0xf3) != 0 {
		return fmt.Errorf("%w: clipboard channel flags=%#08x total=%d fragment=%d", errProtocol, flags, total, len(p))
	}
	if total < 8 || total > maxClipboardMessage {
		return fmt.Errorf("%w: clipboard channel length=%d limit=%d flags=%#08x fragment=%d", errProtocol, total, maxClipboardMessage, flags, len(p))
	}
	if flags&1 != 0 {
		if s.total != 0 {
			return fmt.Errorf("%w: overlapping clipboard fragments", errProtocol)
		}
		s.total, s.buf = total, s.buf[:0]
	}
	if s.total != total || len(p) > total-len(s.buf) {
		return fmt.Errorf("%w: clipboard fragment length", errProtocol)
	}
	s.buf = append(s.buf, p...)
	if flags&2 == 0 {
		return nil
	}
	if len(s.buf) != total {
		return fmt.Errorf("%w: incomplete clipboard message", errProtocol)
	}
	s.total = 0
	return c.handleClipboard(s.buf)
}

func (c *client) handleClipboard(p []byte) error {
	if len(p) < 8 || len(p) > maxClipboardMessage {
		return fmt.Errorf("%w: clipboard PDU length", errProtocol)
	}
	kind, flags := le.Uint16(p), le.Uint16(p[2:])
	declared, actual := uint64(le.Uint32(p[4:])), uint64(len(p)-8)
	if declared != actual {
		// mstsc can append an unused DWORD outside dataLen, and it need not
		// be zero. Only accept that exact suffix after VC reassembly; consume
		// the declared payload without interpreting or copying the suffix.
		if kind != clipFileContentsResponse || declared > actual || actual-declared != 4 {
			return fmt.Errorf("%w: clipboard PDU length (type=%d flags=%#x declared=%d received=%d)",
				errProtocol, kind, flags, declared, actual)
		}
	}
	if !c.clipboardTextAllowed(clipboard.ClientToServer) && !c.clipboardTextAllowed(clipboard.ServerToClient) && !c.clipboardFilesAllowed() && !c.clipboardRichTextAllowedAny() && !c.clipboardImagesAllowedAny() {
		return nil
	}
	p = p[8 : 8+int(declared)]
	s := &c.clipboard
	s.mu.Lock()
	defer s.mu.Unlock()
	if declared != actual && !s.filePaddingLogged {
		logging.Infof("rdp: clipboard file response: ignored 4 trailing bytes outside declared payload")
		s.filePaddingLogged = true
	}
	select {
	case <-c.done:
		return nil
	default:
	}
	if !s.started {
		return fmt.Errorf("%w: clipboard data before initialization", errProtocol)
	}
	switch kind {
	case clipCapabilities:
		capabilities, err := clipboardCapabilityFlags(p)
		if err != nil || s.peerReady {
			return fmt.Errorf("%w: clipboard capabilities", errProtocol)
		}
		s.longNames = capabilities&2 != 0
		s.peerFiles = capabilities&clipStreamFiles != 0
		if c.clipboardFilesAllowed() {
			logging.Infof("rdp: clipboard file capabilities: peer_streaming=%t single_file_limit=%d", s.peerFiles, maxClipboardFileBytes)
		}
	case clipFormatList:
		offer, err := parseClipboardFormats(p, s.longNames, flags&4 != 0)
		if err != nil {
			return c.sendClipboard(clipFormatListResponse, clipFail, nil)
		}
		logging.Infof("rdp: clipboard offer formats: %s", strings.Join(offer.names, ", "))
		s.peerReady = true
		if !c.clipboardTextAllowed(clipboard.ClientToServer) && !c.clipboardReceiveFilesAllowed() && !c.clipboardReceiveRichTextAllowed() && !c.clipboardReceiveImagesAllowed() {
			s.remoteText = false
			return c.sendClipboard(clipFormatListResponse, clipOK, nil)
		}
		fileFormat := uint32(0)
		if c.clipboardReceiveFilesAllowed() && s.peerFiles {
			fileFormat = offer.file
		}
		c.retireRemoteClipboardFiles()
		s.remoteText = offer.text != 0 && c.clipboardTextAllowed(clipboard.ClientToServer)
		s.remoteHTML, s.remoteRTF, s.remoteImage, s.remoteImageEncoding = 0, 0, 0, ""
		if c.clipboardReceiveRichTextAllowed() {
			s.remoteHTML, s.remoteRTF = offer.html, offer.rtf
		}
		if c.clipboardReceiveImagesAllowed() {
			s.remoteImage, s.remoteImageEncoding = offer.image, offer.imageEncoding
		}
		s.remoteFileFormat = fileFormat
		s.generation++
		_, _, revision, readErr := c.s.cfg.Clipboard.ReadText()
		c.clipboardBackendResult(readErr)
		s.offerRevision, s.offerReadable = revision, readErr == nil
		s.localAvailable, s.localHTML, s.localRTF, s.localImage, s.listAccepted = false, nil, nil, nil, false
		s.localFiles.close()
		s.localFiles = nil
		if err := c.sendClipboard(clipFormatListResponse, clipOK, nil); err != nil {
			return err
		}
		if (s.remoteText || s.remoteHTML != 0 || s.remoteRTF != 0 || s.remoteImage != 0 || s.remoteFileFormat != 0) && !s.requestPending {
			return c.requestClipboardData()
		}
	case clipFormatListResponse:
		if len(p) != 0 || (flags != clipOK && flags != clipFail) {
			return fmt.Errorf("%w: clipboard format acknowledgement", errProtocol)
		}
		if s.listPending {
			s.listPending, s.listAccepted = false, flags == clipOK
		}
	case clipDataRequest:
		if len(p) != 4 {
			return fmt.Errorf("%w: clipboard data request", errProtocol)
		}
		if le.Uint32(p) == clipFileGroupDescriptor && c.clipboardSendFilesAllowed() && s.peerFiles && s.localFiles != nil && s.listAccepted {
			_, _, revision, err := c.s.cfg.Clipboard.ReadText()
			c.clipboardBackendResult(err)
			if err != nil || revision != s.revision {
				return c.sendClipboard(clipDataResponse, clipFail, nil)
			}
			return c.sendClipboard(clipDataResponse, clipOK, s.localFiles.data)
		}
		format := le.Uint32(p)
		if !s.listAccepted || !c.clipboardSendFormatAllowed(format) {
			return c.sendClipboard(clipDataResponse, clipFail, nil)
		}
		switch format {
		case clipUnicodeText:
			if !s.localAvailable {
				return c.sendClipboard(clipDataResponse, clipFail, nil)
			}
			data, err := clipboardEncodeText(s.localText)
			if err != nil {
				return c.sendClipboard(clipDataResponse, clipFail, nil)
			}
			return c.sendClipboard(clipDataResponse, clipOK, data)
		case clipHTML:
			if len(s.localHTML) == 0 {
				return c.sendClipboard(clipDataResponse, clipFail, nil)
			}
			return c.sendClipboard(clipDataResponse, clipOK, clipboardEncodeHTML(s.localHTML))
		case clipRTF:
			if len(s.localRTF) == 0 {
				return c.sendClipboard(clipDataResponse, clipFail, nil)
			}
			return c.sendClipboard(clipDataResponse, clipOK, append([]byte(nil), s.localRTF...))
		case clipDIBV5:
			if len(s.localImage) == 0 {
				return c.sendClipboard(clipDataResponse, clipFail, nil)
			}
			data, err := pngToDIBV5(s.localImage)
			if err != nil {
				return c.sendClipboard(clipDataResponse, clipFail, nil)
			}
			return c.sendClipboard(clipDataResponse, clipOK, data)
		}
		return c.sendClipboard(clipDataResponse, clipFail, nil)
	case clipDataResponse:
		if flags != clipOK && flags != clipFail {
			return fmt.Errorf("%w: clipboard data response flags", errProtocol)
		}
		if !c.clipboardReceiveFormatAllowed(s.requestFormat) {
			s.requestPending = false
			return nil
		}
		if !s.requestPending {
			return nil // Ignore late/unsolicited data, never write it to the host.
		}
		s.requestPending = false
		if s.requestGeneration != s.generation {
			if s.remoteText || s.remoteHTML != 0 || s.remoteRTF != 0 || s.remoteImage != 0 || s.remoteFileFormat != 0 {
				return c.requestClipboardData()
			}
			return nil
		}
		if flags == clipFail {
			return nil
		}
		_, _, revision, err := c.s.cfg.Clipboard.ReadText()
		c.clipboardBackendResult(err)
		if err != nil || revision != s.requestRevision {
			return nil // A newer host copy wins over this delayed response.
		}
		select {
		case <-c.done:
			return nil
		default:
		}
		if s.requestFormat == s.remoteFileFormat {
			return c.publishRemoteClipboardFiles(p)
		}
		format := s.requestFormat
		if format == s.remoteImage && s.remoteImageEncoding != "" {
			var png []byte
			png, err = encodedClipboardImageToPNG(p, s.remoteImageEncoding)
			if err == nil {
				revision, err = c.s.cfg.Clipboard.(capture.ImageClipboard).WriteImage(png)
			}
		} else if format == s.remoteHTML {
			format = clipHTML
			revision, err = c.writeRemoteClipboardData(format, p)
		} else if format == s.remoteRTF {
			format = clipRTF
			revision, err = c.writeRemoteClipboardData(format, p)
		} else {
			revision, err = c.writeRemoteClipboardData(format, p)
		}
		c.clipboardBackendResult(err)
		if err == nil {
			s.revision, s.haveRevision = revision, true
		} else {
			logging.Infof("rdp: clipboard format %#x rejected: %v", s.requestFormat, err)
		}
	case clipFileContentsRequest:
		return c.handleClipboardFileRequest(p)
	case clipFileContentsResponse:
		return c.handleClipboardFileResponse(flags, p)
	case 6: // No direct shared-path access; only negotiated file streams are used.
	default:
		// Unsupported extensions do not affect the text channel.
	}
	return nil
}

// Called with the clipboard mutex held. Only one request may be outstanding:
// the protocol has no request IDs, so changed offers wait for the old reply.
func (c *client) requestClipboardData() error {
	s := &c.clipboard
	format := uint32(clipUnicodeText)
	// Browsers commonly offer a copied image both as a virtual file and as a
	// bitmap. Prefer the bitmap: native paste targets can consume it directly,
	// while file promises require a Finder-style coordinated read.
	if s.remoteImage != 0 && c.clipboardReceiveImagesAllowed() {
		format = s.remoteImage
	} else if s.remoteHTML != 0 && c.clipboardReceiveRichTextAllowed() {
		format = s.remoteHTML
	} else if s.remoteRTF != 0 && c.clipboardReceiveRichTextAllowed() {
		format = s.remoteRTF
	} else if s.remoteFileFormat != 0 && c.clipboardReceiveFilesAllowed() {
		format = s.remoteFileFormat
	} else if !s.remoteText || !c.clipboardTextAllowed(clipboard.ClientToServer) {
		return nil
	}
	_, _, revision, err := c.s.cfg.Clipboard.ReadText()
	c.clipboardBackendResult(err)
	if err != nil {
		return nil
	}
	if !s.offerReadable || revision != s.offerRevision {
		return nil // A host copy also supersedes an offer queued behind a request.
	}
	s.requestPending, s.requestRevision = true, revision
	s.requestGeneration, s.requestTime = s.generation, time.Now()
	s.requestFormat = format
	return c.sendClipboard(clipDataRequest, 0, u32(format))
}

func (c *client) clipboardSendFormatAllowed(format uint32) bool {
	switch format {
	case clipUnicodeText:
		return c.clipboardTextAllowed(clipboard.ServerToClient)
	case clipHTML, clipRTF:
		return c.clipboardSendRichTextAllowed()
	case clipDIB, clipDIBV5:
		return c.clipboardSendImagesAllowed()
	}
	return false
}

func (c *client) clipboardReceiveFormatAllowed(format uint32) bool {
	if format == c.clipboard.remoteHTML || format == c.clipboard.remoteRTF {
		return c.clipboardReceiveRichTextAllowed()
	}
	if format == c.clipboard.remoteImage {
		return c.clipboardReceiveImagesAllowed()
	}
	switch format {
	case clipUnicodeText:
		return c.clipboardTextAllowed(clipboard.ClientToServer)
	case clipHTML, clipRTF:
		return c.clipboardReceiveRichTextAllowed()
	case clipDIB, clipDIBV5:
		return c.clipboardReceiveImagesAllowed()
	default:
		return format != 0 && format == c.clipboard.remoteFileFormat && c.clipboardReceiveFilesAllowed()
	}
}

// Called with the clipboard mutex held after checking the host revision.
func (c *client) writeRemoteClipboardData(format uint32, data []byte) (uint64, error) {
	switch format {
	case clipUnicodeText:
		text, err := clipboardDecodeText(data)
		if err != nil {
			return 0, err
		}
		return c.s.cfg.Clipboard.WriteText(text)
	case clipHTML:
		html, err := clipboardDecodeHTML(data)
		if err != nil {
			return 0, err
		}
		return c.s.cfg.Clipboard.(capture.RichTextClipboard).WriteRichText(html, nil)
	case clipRTF:
		if len(data) == 0 || len(data) > maxClipboardMessage {
			return 0, errProtocol
		}
		return c.s.cfg.Clipboard.(capture.RichTextClipboard).WriteRichText(nil, append([]byte(nil), data...))
	case clipDIB, clipDIBV5:
		png, err := dibToPNG(data)
		if err != nil {
			return 0, err
		}
		return c.s.cfg.Clipboard.(capture.ImageClipboard).WriteImage(png)
	}
	return 0, errProtocol
}

// Called with the clipboard mutex held; report failures once until recovery.
func (c *client) clipboardBackendResult(err error) {
	if err != nil && !c.clipboard.backendFailed {
		logging.Infof("rdp: host clipboard: %v", err)
	}
	c.clipboard.backendFailed = err != nil
}

func clipboardLongNames(p []byte) (bool, error) {
	flags, err := clipboardCapabilityFlags(p)
	return flags&2 != 0, err
}

func clipboardCapabilityFlags(p []byte) (uint32, error) {
	if len(p) < 4 {
		return 0, errProtocol
	}
	count := int(le.Uint16(p))
	p = p[4:]
	var flags uint32
	for i := 0; i < count; i++ {
		if len(p) < 4 {
			return 0, errProtocol
		}
		kind, size := le.Uint16(p), int(le.Uint16(p[2:]))
		if size < 4 || size > len(p) {
			return 0, errProtocol
		}
		if kind == 1 {
			if size < 12 {
				return 0, errProtocol
			}
			flags = le.Uint32(p[8:])
		}
		p = p[size:]
	}
	if len(p) != 0 {
		return 0, errProtocol
	}
	return flags, nil
}

func clipboardHasUnicode(p []byte, long bool) (bool, error) {
	formats, err := parseClipboardFormats(p, long, false)
	return formats.text != 0, err
}

type clipboardFormat struct {
	id   uint32
	name string
}

type clipboardFormats struct {
	text, html, rtf, image, file uint32
	imageEncoding                string
	names                        []string
}

func clipboardFormatList(long bool, formats []clipboardFormat) []byte {
	var out []byte
	for _, format := range formats {
		out = append(out, u32(format.id)...)
		if long {
			name, _ := clipboardEncodeText(format.name)
			out = append(out, name...)
			continue
		}
		name, _ := clipboardEncodeText(format.name)
		field := make([]byte, 32)
		copy(field, name)
		out = append(out, field...)
	}
	return out
}

func parseClipboardFormats(p []byte, long, ascii bool) (clipboardFormats, error) {
	var formats clipboardFormats
	for len(p) != 0 {
		if len(p) < 4 {
			return formats, errProtocol
		}
		id := le.Uint32(p)
		name := ""
		if !long {
			if len(p) < 36 {
				return formats, errProtocol
			}
			if ascii {
				name = strings.SplitN(string(p[4:36]), "\x00", 2)[0]
			} else {
				var err error
				name, err = clipboardDecodeText(p[4:36])
				if err != nil {
					return formats, err
				}
			}
			p = p[36:]
		} else {
			p = p[4:]
			end := -1
			for i := 0; i+1 < len(p); i += 2 {
				if le.Uint16(p[i:]) == 0 {
					end = i + 2
					break
				}
			}
			if end < 0 {
				return formats, errProtocol
			}
			var err error
			name, err = clipboardDecodeText(p[:end])
			if err != nil {
				return formats, err
			}
			p = p[end:]
		}
		formats.names = append(formats.names, fmt.Sprintf("%#x:%q", id, name))
		switch {
		case id == clipUnicodeText:
			formats.text = id
		case id == clipDIBV5 && !strings.EqualFold(formats.imageEncoding, clipPNGName):
			formats.image = id // Prefer the alpha-preserving DIBV5 value.
			formats.imageEncoding = ""
		case id == clipDIB && formats.image == 0:
			formats.image = id
		case strings.EqualFold(name, clipPNGName):
			if id < 0xc000 {
				return formats, errProtocol
			}
			// Prefer PNG when supplied: it avoids a lossy or oversized DIB
			// conversion by clients such as Windows Snipping Tool.
			formats.image, formats.imageEncoding = id, clipPNGName
		case isClipboardRasterImageName(name) && formats.image == 0:
			if id < 0xc000 {
				return formats, errProtocol
			}
			formats.image, formats.imageEncoding = id, name
		case strings.EqualFold(name, clipHTMLName):
			if id < 0xc000 {
				return formats, errProtocol
			}
			formats.html = id
		case strings.EqualFold(name, clipRTFName):
			if id < 0xc000 {
				return formats, errProtocol
			}
			formats.rtf = id
		case name == clipFileGroupName:
			if id < 0xc000 || formats.file != 0 {
				return formats, errProtocol
			}
			formats.file = id
		}
	}
	return formats, nil
}

func isClipboardRasterImageName(name string) bool {
	return strings.EqualFold(name, clipJPEGName) || strings.EqualFold(name, clipJFIFName) || strings.EqualFold(name, clipGIFName)
}

func clipboardEncodeText(text string) ([]byte, error) {
	if len(text) > 2*maxClipboardMessage || !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return nil, fmt.Errorf("invalid clipboard text")
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", "\r\n")
	units := utf16.Encode([]rune(text))
	if (len(units)+1)*2 > maxClipboardMessage-8 {
		return nil, fmt.Errorf("clipboard text exceeds channel limit")
	}
	p := make([]byte, (len(units)+1)*2)
	for i, v := range units {
		le.PutUint16(p[i*2:], v)
	}
	return p, nil
}

func clipboardDecodeText(p []byte) (string, error) {
	if len(p) < 2 || len(p)%2 != 0 {
		return "", errProtocol
	}
	var units []uint16
	for len(p) > 0 {
		v := le.Uint16(p)
		p = p[2:]
		if v == 0 {
			for i, u := range units {
				if u >= 0xd800 && u <= 0xdbff && (i+1 == len(units) || units[i+1] < 0xdc00 || units[i+1] > 0xdfff) ||
					u >= 0xdc00 && u <= 0xdfff && (i == 0 || units[i-1] < 0xd800 || units[i-1] > 0xdbff) {
					return "", errProtocol
				}
			}
			return strings.ReplaceAll(string(utf16.Decode(units)), "\r\n", "\n"), nil
		}
		units = append(units, v)
	}
	return "", errProtocol
}
