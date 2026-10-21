//go:build cgo && !ffmpeg && (linux || windows || darwin)

package jpegcodec

/*
#cgo linux LDFLAGS: -ldl
#include <stdlib.h>
#include "transform_turbo.h"
*/
import "C"

import (
	"fmt"
	"image"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"unsafe"
)

var losslessTurboAvailable = sync.OnceValue(func() bool {
	names := []string{"libturbojpeg.so.0", "libturbojpeg.so"}
	if runtime.GOOS == "windows" {
		names = []string{"turbojpeg.dll", "libturbojpeg.dll"}
	}
	if runtime.GOOS == "darwin" {
		names = []string{"libturbojpeg.0.dylib", "libturbojpeg.dylib"}
	}
	dirs := []string{}
	if dir := os.Getenv("ANYKVM_JPEG_LIBDIR"); dir != "" {
		dirs = append(dirs, dir)
	}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Join(filepath.Dir(exe), "lib"), filepath.Dir(exe))
	}
	if runtime.GOOS == "darwin" {
		dirs = append(dirs, "/opt/homebrew/opt/jpeg-turbo/lib", "/usr/local/opt/jpeg-turbo/lib")
	}
	dirs = append(dirs, "")
	for _, dir := range dirs {
		for _, name := range names {
			path := name
			if dir != "" {
				var err error
				path, err = filepath.Abs(filepath.Join(dir, name))
				if err != nil {
					continue
				}
			}
			p := C.CString(path)
			ok := C.bk_jt_load(p) != 0
			C.free(unsafe.Pointer(p))
			if ok {
				return true
			}
		}
	}
	return false
})

// LosslessJPEGTiles crops in the quantized DCT domain, retaining source quality.
// All transforms share one input coefficient decode. Returned bytes are owned
// by Go. This optional runtime is also usable with the FFmpeg pixel backend.
func LosslessJPEGTiles(data []byte, tileSize int) ([]JPEGTile, int, int, error) {
	if len(data) == 0 || len(data) > 128<<20 || tileSize < 32 || tileSize > 2048 || tileSize%32 != 0 {
		return nil, 0, 0, fmt.Errorf("invalid lossless JPEG tile input")
	}
	if !losslessTurboAvailable() {
		return nil, 0, 0, fmt.Errorf("TurboJPEG lossless transform runtime unavailable")
	}
	var result *C.bk_jpeg_tile
	var count, width, height C.int
	var message [512]C.char
	ok := C.bk_jt_tiles((*C.uchar)(unsafe.Pointer(&data[0])), C.size_t(len(data)), C.int(tileSize), &result, &count, &width, &height, &message[0], C.size_t(len(message)))
	runtime.KeepAlive(data)
	defer C.bk_jt_tiles_free(result, count)
	if ok == 0 {
		return nil, 0, 0, fmt.Errorf("lossless JPEG crop: %s", C.GoString(&message[0]))
	}
	if result == nil || count < 1 || count > 65536 {
		return nil, 0, 0, fmt.Errorf("invalid JPEG crop result")
	}
	tiles := make([]JPEGTile, int(count))
	for i, item := range unsafe.Slice(result, int(count)) {
		if item.data == nil || item.size < 1 || item.size > 128<<20 {
			return nil, 0, 0, fmt.Errorf("invalid JPEG crop payload")
		}
		tiles[i] = JPEGTile{Rect: image.Rect(int(item.x), int(item.y), int(item.x+item.w), int(item.y+item.h)), Data: C.GoBytes(unsafe.Pointer(item.data), C.int(item.size))}
	}
	return tiles, int(width), int(height), nil
}
