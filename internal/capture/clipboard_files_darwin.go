//go:build darwin

package capture

/*
#include "clipboard_darwin.h"
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/unix"
	"golang.org/x/text/unicode/norm"

	"anykvm/internal/logging"
)

type deferredClipboardOffer struct {
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	files   []ClipboardFile
	handles []*os.File
	root    *os.Root
	receive func(context.Context, int, *os.File) error
	ready   bool
	err     error
}

var deferredClipboardOffers sync.Map
var deferredClipboardID atomic.Uint64

// Run after protocol shutdown while the macOS event loop still processes
// clipboard operations. Also flush completed offers in their quiet period.
func shutdownDeferredClipboardFiles() {
	deferredClipboardOffers.Range(func(key, value any) bool {
		deferredClipboardOffers.Delete(key)
		value.(*deferredClipboardOffer).dispose()
		return true
	})
	C.bk_clipboard_shutdown_files()
}

func clipboardTemporaryBase() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("anykvm-clipboard-%d", os.Getuid()))
}

// Only remove owned offer directories whose lifetime lock is not held by any
// process. This also cleans crash leftovers without touching active instances.
func cleanClipboardTemporaryFiles(base string) {
	entries, _ := os.ReadDir(base)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "offer-") {
			continue
		}
		path := filepath.Join(base, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			continue
		}
		fd, err := unix.Open(filepath.Join(path, ".lock"), unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			continue // Creation may still be in progress; never remove an unlocked offer.
		}
		var st unix.Stat_t
		if unix.Fstat(fd, &st) == nil && st.Uid == uint32(os.Getuid()) && st.Mode&unix.S_IFMT == unix.S_IFREG &&
			unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) == nil {
			if err := os.RemoveAll(path); err == nil {
				logging.Infof("clipboard: removed abandoned temporary directory %s", path)
			}
		}
		unix.Close(fd)
	}
}

func newDeferredClipboardOffer(ctx context.Context, files []ClipboardFile,
	receive func(context.Context, int, *os.File) error) (_ *deferredClipboardOffer, roots []string, lock *os.File, err error) {
	if len(files) == 0 || len(files) > 1024 || receive == nil {
		return nil, nil, nil, errors.New("invalid deferred clipboard collection")
	}
	base := clipboardTemporaryBase()
	if err := os.MkdirAll(base, 0700); err != nil {
		return nil, nil, nil, err
	}
	var st unix.Stat_t
	if err := unix.Lstat(base, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR ||
		st.Uid != uint32(os.Getuid()) || st.Mode&0777 != 0700 {
		return nil, nil, nil, errors.New("clipboard temporary base must be a private owned directory")
	}
	guardFD, err := unix.Open(filepath.Join(base, ".guard"), unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, nil, nil, err
	}
	defer unix.Close(guardFD)
	if err := unix.Flock(guardFD, unix.LOCK_EX); err != nil {
		return nil, nil, nil, err
	}
	defer unix.Flock(guardFD, unix.LOCK_UN)
	cleanClipboardTemporaryFiles(base)
	directory, err := os.MkdirTemp(base, "offer-")
	if err != nil {
		return nil, nil, nil, err
	}
	offer := &deferredClipboardOffer{files: append([]ClipboardFile(nil), files...), receive: receive}
	offer.ctx, offer.cancel = context.WithCancel(ctx)
	defer func() {
		if err != nil {
			offer.dispose()
			if lock != nil {
				lock.Close()
			}
			os.RemoveAll(directory)
		}
	}()
	lock, err = os.OpenFile(filepath.Join(directory, ".lock"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, nil, nil, err
	}
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, nil, lock, err
	}
	offer.root, err = os.OpenRoot(directory)
	if err != nil {
		return nil, nil, lock, err
	}
	seen, tops := make(map[string]bool), make(map[string]bool)
	for _, entry := range files {
		name := entry.Name
		key := strings.ToLower(norm.NFC.String(name))
		if !utf8.ValidString(name) || !filepath.IsLocal(name) || filepath.Clean(name) != name || strings.ContainsAny(name, "\\\x00") ||
			name == "." || strings.Split(name, "/")[0] == ".lock" || seen[key] || entry.Size < -1 {
			return nil, nil, lock, errors.New("invalid deferred clipboard path")
		}
		seen[key] = true
		parent := filepath.Dir(name)
		if entry.Directory {
			parent = name
		}
		// The staging directory is private. File handles are pinned below, and
		// rooted identity checks prevent a replacement from redirecting writes.
		if err = os.MkdirAll(filepath.Join(directory, parent), 0700); err != nil {
			return nil, nil, lock, err
		}
		var file *os.File
		if !entry.Directory {
			file, err = offer.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
			if err != nil {
				return nil, nil, lock, err
			}
		}
		offer.handles = append(offer.handles, file)
		top := strings.Split(name, "/")[0]
		if !tops[top] {
			roots = append(roots, filepath.Join(directory, top))
			tops[top] = true
		}
	}
	return offer, roots, lock, nil
}

func (o *deferredClipboardOffer) dispose() {
	o.cancel()
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, file := range o.handles {
		if file != nil {
			file.Close()
		}
	}
	if o.root != nil {
		o.root.Close()
	}
}

func (o *deferredClipboardOffer) materialize() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ready || o.err != nil {
		return o.err
	}
	for i, entry := range o.files {
		if err := o.ctx.Err(); err != nil {
			o.err = err
			break
		}
		if entry.Directory {
			continue
		}
		file := o.handles[i]
		info, err := file.Stat()
		pathInfo, pathErr := o.root.Lstat(entry.Name)
		if err != nil || pathErr != nil || !os.SameFile(info, pathInfo) || !info.Mode().IsRegular() {
			o.err = errors.New("clipboard placeholder changed before transfer")
			break
		}
		if err := o.receive(o.ctx, i, file); err != nil {
			o.err = err
			break
		}
		info, err = file.Stat()
		if err != nil || entry.Size >= 0 && info.Size() != entry.Size {
			o.err = errors.New("clipboard transfer returned an incomplete file")
			break
		}
		if err := file.Sync(); err != nil {
			o.err = err
			break
		}
	}
	if o.err == nil {
		o.err = o.ctx.Err()
	}
	if o.err != nil {
		return o.err
	}
	for i := len(o.files) - 1; i >= 0; i-- {
		entry := o.files[i]
		if !entry.Modified.IsZero() {
			_ = os.Chtimes(filepath.Join(o.root.Name(), entry.Name), entry.Modified, entry.Modified)
		}
	}
	o.ready = true
	return nil
}

//export bk_clipboard_materialize
func bk_clipboard_materialize(id C.uint64_t) C.int {
	value, ok := deferredClipboardOffers.Load(uint64(id))
	if !ok {
		return 0
	}
	if err := value.(*deferredClipboardOffer).materialize(); err != nil {
		logging.Infof("clipboard: deferred file transfer: %v", err)
		return 0
	}
	return 1
}

func (systemClipboard) WriteDeferredFiles(ctx context.Context, expectedRevision uint64, files []ClipboardFile,
	receive func(context.Context, int, *os.File) error) (uint64, func(), error) {
	offer, paths, lock, err := newDeferredClipboardOffer(ctx, files, receive)
	if err != nil {
		return 0, nil, err
	}
	directory := offer.root.Name()
	id := deferredClipboardID.Add(1)
	allPaths := append([]string(nil), paths...)
	seenPaths := make(map[string]bool)
	for _, path := range allPaths {
		seenPaths[path] = true
	}
	for _, entry := range offer.files {
		path := filepath.Join(directory, entry.Name)
		if !seenPaths[path] {
			allPaths = append(allPaths, path)
			seenPaths[path] = true
		}
	}
	ready := true
	for _, entry := range offer.files {
		if !entry.Directory && entry.Size != 0 {
			ready = false
			break
		}
	}
	if ready {
		offer.ready = true
		for i := len(offer.files) - 1; i >= 0; i-- {
			entry := offer.files[i]
			if !entry.Modified.IsZero() {
				_ = os.Chtimes(filepath.Join(directory, entry.Name), entry.Modified, entry.Modified)
			}
		}
	}
	data, _ := json.Marshal(struct {
		Roots []string `json:"roots"`
		Paths []string `json:"paths"`
		Ready bool     `json:"ready"`
	}{paths, allPaths, ready})
	deferredClipboardOffers.Store(id, offer)
	encoded := C.CString(string(data))
	defer C.free(unsafe.Pointer(encoded))
	var revision C.uint64_t
	systemClipboardMu.Lock()
	status := C.bk_clipboard_publish_files(encoded, C.size_t(len(data)), C.uint64_t(id), C.int(lock.Fd()),
		C.uint64_t(expectedRevision), &revision)
	systemClipboardMu.Unlock()
	lock.Close() // The native lease duplicates the locked descriptor on success.
	if status != 1 {
		deferredClipboardOffers.Delete(id)
		offer.dispose()
		os.RemoveAll(directory)
		return uint64(revision), nil, errors.New("macOS deferred file publication failed or clipboard changed")
	}
	logging.Infof("clipboard: deferred file offer %s (%d entries, no content downloaded)", directory, len(files))
	var once sync.Once
	release := func() {
		once.Do(func() {
			offer.cancel()
			deferredClipboardOffers.Delete(id)
			C.bk_clipboard_clear_file_offer(C.uint64_t(id))
			// Do not wait on a network callback while the protocol mutex is held.
			go func() {
				C.bk_clipboard_retire_files(C.uint64_t(id))
				offer.dispose()
			}()
		})
	}
	return uint64(revision), release, nil
}
