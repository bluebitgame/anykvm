//go:build darwin

package capture

/*
#cgo LDFLAGS: -framework AppKit
#include "clipboard_darwin.h"
#include <stdlib.h>
*/
import "C"

import (
	"encoding/json"
	"errors"
	"runtime"
	"sync"
	"unsafe"
)

const maxNativeClipboardBytes = 16 << 20

func (systemClipboard) ReadFiles() ([]string, bool, uint64, error) {
	systemClipboardMu.Lock()
	defer systemClipboardMu.Unlock()
	var data *C.char
	var length C.size_t
	var revision C.uint64_t
	runtime.LockOSThread()
	status := C.bk_clipboard_read_files(&data, &length, &revision)
	runtime.UnlockOSThread()
	if status == 2 {
		return nil, false, uint64(revision), ErrClipboardFilesPending
	}
	if status < 0 {
		return nil, false, uint64(revision), errors.New("macOS file clipboard read failed")
	}
	if data == nil {
		return nil, false, uint64(revision), nil
	}
	defer C.free(unsafe.Pointer(data))
	var paths []string
	err := json.Unmarshal(C.GoBytes(unsafe.Pointer(data), C.int(length)), &paths)
	return paths, len(paths) > 0 && err == nil, uint64(revision), err
}

type systemClipboard struct{}

var systemClipboardMu sync.Mutex

func NewClipboard() Clipboard { return systemClipboard{} }

func (systemClipboard) ReadText() (string, bool, uint64, error) {
	systemClipboardMu.Lock()
	defer systemClipboardMu.Unlock()
	var text *C.char
	var length C.size_t
	var revision C.uint64_t
	runtime.LockOSThread()
	status := C.bk_clipboard_read(&text, &length, &revision)
	runtime.UnlockOSThread()
	if status < 0 {
		return "", false, uint64(revision), errors.New("macOS clipboard read failed")
	}
	if text == nil {
		return "", false, uint64(revision), nil
	}
	defer C.free(unsafe.Pointer(text))
	return C.GoStringN(text, C.int(length)), true, uint64(revision), nil
}

func (systemClipboard) WriteText(text string) (uint64, error) {
	systemClipboardMu.Lock()
	defer systemClipboardMu.Unlock()
	bytes := C.CString(text)
	defer C.free(unsafe.Pointer(bytes))
	var revision C.uint64_t
	runtime.LockOSThread()
	status := C.bk_clipboard_write(bytes, C.size_t(len(text)), &revision)
	runtime.UnlockOSThread()
	if status == 0 {
		return uint64(revision), errors.New("macOS clipboard write failed")
	}
	return uint64(revision), nil
}

func (systemClipboard) ReadRichText() (html, rtf []byte, revision uint64, err error) {
	systemClipboardMu.Lock()
	defer systemClipboardMu.Unlock()
	var htmlData, rtfData *C.char
	var htmlLength, rtfLength C.size_t
	var change C.uint64_t
	runtime.LockOSThread()
	status := C.bk_clipboard_read_rich(&htmlData, &htmlLength, &rtfData, &rtfLength, &change)
	runtime.UnlockOSThread()
	if htmlData != nil {
		defer C.free(unsafe.Pointer(htmlData))
		html = C.GoBytes(unsafe.Pointer(htmlData), C.int(htmlLength))
	}
	if rtfData != nil {
		defer C.free(unsafe.Pointer(rtfData))
		rtf = C.GoBytes(unsafe.Pointer(rtfData), C.int(rtfLength))
	}
	if status < 0 {
		return nil, nil, uint64(change), errors.New("macOS rich clipboard read failed")
	}
	return html, rtf, uint64(change), nil
}

func (systemClipboard) WriteRichText(html, rtf []byte) (uint64, error) {
	if len(html) == 0 && len(rtf) == 0 || len(html) > maxNativeClipboardBytes || len(rtf) > maxNativeClipboardBytes {
		return 0, errors.New("invalid macOS rich clipboard data")
	}
	systemClipboardMu.Lock()
	defer systemClipboardMu.Unlock()
	var htmlData, rtfData unsafe.Pointer
	if len(html) != 0 {
		htmlData = C.CBytes(html)
		defer C.free(htmlData)
	}
	if len(rtf) != 0 {
		rtfData = C.CBytes(rtf)
		defer C.free(rtfData)
	}
	var revision C.uint64_t
	runtime.LockOSThread()
	status := C.bk_clipboard_write_rich((*C.char)(htmlData), C.size_t(len(html)), (*C.char)(rtfData), C.size_t(len(rtf)), &revision)
	runtime.UnlockOSThread()
	if status == 0 {
		return uint64(revision), errors.New("macOS rich clipboard write failed")
	}
	return uint64(revision), nil
}

func (systemClipboard) ReadImage() (png []byte, revision uint64, err error) {
	systemClipboardMu.Lock()
	defer systemClipboardMu.Unlock()
	var data *C.char
	var length C.size_t
	var change C.uint64_t
	runtime.LockOSThread()
	status := C.bk_clipboard_read_image(&data, &length, &change)
	runtime.UnlockOSThread()
	if data != nil {
		defer C.free(unsafe.Pointer(data))
		png = C.GoBytes(unsafe.Pointer(data), C.int(length))
	}
	if status < 0 {
		return nil, uint64(change), errors.New("macOS image clipboard read failed")
	}
	return png, uint64(change), nil
}

func (systemClipboard) WriteImage(png []byte) (uint64, error) {
	if len(png) == 0 || len(png) > maxNativeClipboardBytes {
		return 0, errors.New("invalid macOS image clipboard data")
	}
	systemClipboardMu.Lock()
	defer systemClipboardMu.Unlock()
	data := C.CBytes(png)
	defer C.free(data)
	var revision C.uint64_t
	runtime.LockOSThread()
	status := C.bk_clipboard_write_image((*C.char)(data), C.size_t(len(png)), &revision)
	runtime.UnlockOSThread()
	if status == 0 {
		return uint64(revision), errors.New("macOS image clipboard write failed")
	}
	return uint64(revision), nil
}
