//go:build cgo && !ffmpeg && (linux || windows || darwin)

package jpegcodec

/*
#cgo linux LDFLAGS: -ldl
#include <stdlib.h>
#include "turbo.h"
*/
import "C"

import (
	"anykvm/internal/framebuf"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"unsafe"
)

var turbo struct {
	sync.Once
	loaded bool
}

func loadTurbo() bool {
	turbo.Do(func() {
		names := []string{"libturbojpeg.so.0", "libturbojpeg.so"}
		if runtime.GOOS == "windows" {
			names = []string{"turbojpeg.dll", "libturbojpeg.dll"}
		} else if runtime.GOOS == "darwin" { // macOS Intel uses the portable backend by default.
			names = []string{"libturbojpeg.0.dylib", "libturbojpeg.dylib"}
		}
		dirs := []string{}
		if directory := os.Getenv("ANYKVM_JPEG_LIBDIR"); directory != "" {
			dirs = append(dirs, directory)
		}
		if executable, err := os.Executable(); err == nil {
			dirs = append(dirs, filepath.Join(filepath.Dir(executable), "lib"), filepath.Dir(executable))
		}
		if runtime.GOOS == "darwin" {
			dirs = append(dirs, "/opt/homebrew/opt/jpeg-turbo/lib", "/usr/local/opt/jpeg-turbo/lib")
		}
		dirs = append(dirs, "") // Normal platform library search paths.
		for _, directory := range dirs {
			for _, name := range names {
				path := name
				if directory != "" {
					path = filepath.Join(directory, name)
					absolute, err := filepath.Abs(path)
					if err != nil {
						continue
					}
					path = absolute
				}
				candidate := C.CString(path)
				loaded := C.bk_tj_load(candidate) != 0
				C.free(unsafe.Pointer(candidate))
				if loaded {
					turbo.loaded = true
					return
				}
			}
		}
	})
	return turbo.loaded
}

func Backend() string {
	if loadTurbo() {
		return "libjpeg-turbo"
	}
	return fallbackBackend()
}

func encodeBGRA(pixels []byte, width, height, quality int) ([]byte, error) {
	b, err := encodeBGRAOwned(pixels, width, height, quality)
	if err != nil {
		return nil, err
	}
	defer b.Release()
	return append([]byte(nil), b.Bytes()...), nil
}
func encodeBGRAOwned(pixels []byte, width, height, quality int) (*framebuf.Buffer, error) {
	if !loadTurbo() {
		return encodeFallbackOwned(pixels, width, height, quality)
	}
	var data *C.uchar
	var size C.ulong
	var message [512]C.char
	ok := C.bk_tj_encode((*C.uchar)(unsafe.Pointer(&pixels[0])), C.int(width), C.int(height), C.int(quality), &data, &size, &message[0], C.size_t(len(message)))
	runtime.KeepAlive(pixels)
	if data != nil {
		defer func() {
			if data != nil {
				C.bk_tj_free(data)
			}
		}()
	}
	if ok == 0 {
		return encodeFallbackOwned(pixels, width, height, quality)
	}
	if data == nil || size < 1 || size > 1<<31-1 {
		return encodeFallbackOwned(pixels, width, height, quality)
	}
	owned := data
	bytes := unsafe.Slice((*byte)(unsafe.Pointer(data)), int(size))
	data = nil
	return framebuf.New(bytes, func() { C.bk_tj_free(owned) }), nil
}

func decodeBGRA(data []byte, width, height int) ([]byte, error) {
	if !loadTurbo() {
		return decodeFallback(data, width, height)
	}
	pixels := make([]byte, width*height*4)
	var message [512]C.char
	ok := C.bk_tj_decode((*C.uchar)(unsafe.Pointer(&data[0])), C.size_t(len(data)), (*C.uchar)(unsafe.Pointer(&pixels[0])), C.int(width), C.int(height), &message[0], C.size_t(len(message)))
	runtime.KeepAlive(data)
	if ok == 0 {
		// TurboJPEG cannot convert every JPEG colorspace to BGRA (for example
		// CMYK/YCCK). Preserve support through the platform fallback.
		return decodeFallback(data, width, height)
	}
	return pixels, nil
}
