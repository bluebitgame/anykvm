package capture

import (
	"context"
	"errors"
	"os"
	"time"
)

// Clipboard provides a revisioned plain-text view of the host clipboard.
// ReadText distinguishes an empty string from a clipboard without text.
// Implementations must allow concurrent calls. WriteText returns the revision
// of its own write so consumers can suppress echoes without hiding later edits.
type Clipboard interface {
	ReadText() (text string, available bool, revision uint64, err error)
	WriteText(text string) (revision uint64, err error)
}

// RichTextClipboard is an optional clipboard capability for HTML and RTF.
// Values are the native format bytes (not a protocol-specific envelope). Text
// and rich text share one revision counter.
type RichTextClipboard interface {
	ReadRichText() (html, rtf []byte, revision uint64, err error)
	WriteRichText(html, rtf []byte) (revision uint64, err error)
}

// ImageClipboard is an optional clipboard capability for PNG images. PNG keeps
// the backend boundary lossless and independent of remote clipboard formats.
// Images share the same revision counter as text and rich text.
type ImageClipboard interface {
	ReadImage() (png []byte, revision uint64, err error)
	WriteImage(png []byte) (revision uint64, err error)
}

// FileClipboard is an optional clipboard capability shared by remote protocols.
// Paths are absolute local filesystem paths, including directories. Text and
// files use the same revision counter. No file contents are read here.
type FileClipboard interface {
	ReadFiles() (paths []string, available bool, revision uint64, err error)
}

// Pending files must not be advertised as zero-byte files by another protocol
// or session while their coordinated read has not materialized the source yet.
var ErrClipboardFilesPending = errors.New("clipboard files await a coordinated read")

// ClipboardFile describes a relative path in an offered collection. Size -1
// means that the receiving protocol must obtain the size when reading begins.
type ClipboardFile struct {
	Name      string
	Directory bool
	Size      int64
	Modified  time.Time
}

// DeferredFileClipboard publishes empty placeholders and invokes receive only
// when a coordinated consumer reads them. The host waits for complete content
// before allowing the read. release cancels pending transfers and retires the
// offer; files remain alive until coordinated readers finish. Other protocols
// can use this capability without depending on RDP or Finder automation.
type DeferredFileClipboard interface {
	WriteDeferredFiles(ctx context.Context, expectedRevision uint64, files []ClipboardFile,
		receive func(context.Context, int, *os.File) error) (revision uint64, release func(), err error)
}
