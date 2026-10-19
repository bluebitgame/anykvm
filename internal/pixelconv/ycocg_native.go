//go:build cgo && (amd64 || arm64) && (darwin || linux || windows)

package pixelconv

/*
#cgo CFLAGS: -O3
#cgo amd64 CFLAGS: -march=x86-64
#include "ycocg_native.h"
*/
import "C"

import (
	"runtime"
	"unsafe"
)

func convertNative(mode int, dst, first, second []byte, fs, ss, w, h int, nv12 bool) {
	var uv *C.uint8_t
	if len(second) != 0 {
		uv = (*C.uint8_t)(unsafe.Pointer(&second[0]))
	}
	kind := 0
	if nv12 {
		kind = 1
	}
	C.bk_ycocg420(C.int(mode), C.int(kind), (*C.uint8_t)(unsafe.Pointer(&dst[0])),
		(*C.uint8_t)(unsafe.Pointer(&first[0])), uv, C.int(fs), C.int(ss), C.int(w), C.int(h))
	runtime.KeepAlive(dst)
	runtime.KeepAlive(first)
	runtime.KeepAlive(second)
}
func deltaNative(mode int, dst, row, prev []byte) {
	if len(row) == 0 {
		return
	}
	C.bk_planar_delta(C.int(mode), (*C.uint8_t)(unsafe.Pointer(&dst[0])),
		(*C.uint8_t)(unsafe.Pointer(&row[0])), (*C.uint8_t)(unsafe.Pointer(&prev[0])), C.int(len(row)))
	runtime.KeepAlive(dst)
	runtime.KeepAlive(row)
	runtime.KeepAlive(prev)
}
