//go:build cgo && arm64 && (darwin || linux || windows)

package pixelops

/*
#cgo CFLAGS: -O3
#include <stdint.h>
int bk_pixels_equal(const uint8_t *,const uint8_t *,int,int,int);
int bk_pixels_run(const uint8_t *,int,int);
void bk_pixels_pack(uint8_t *,const uint8_t *,int);
*/
import "C"

import (
	"runtime"
	"unsafe"
)

func init() { selected = implementation{"NEON", equalBlockNEON, runBGRNEON, packBGRNEON, runByteNEON} }
func equalBlockNEON(a, b []byte, stride, width, height int) bool {
	if width == 0 || height == 0 {
		return true
	}
	result := C.bk_pixels_equal((*C.uint8_t)(unsafe.Pointer(&a[0])), (*C.uint8_t)(unsafe.Pointer(&b[0])), C.int(stride), C.int(width), C.int(height))
	runtime.KeepAlive(a)
	runtime.KeepAlive(b)
	return result != 0
}
func runBGRNEON(p []byte) int  { return runNEON(p, 4) }
func runByteNEON(p []byte) int { return runNEON(p, 1) }
func runNEON(p []byte, unit int) int {
	if len(p) == 0 {
		return 0
	}
	n := C.bk_pixels_run((*C.uint8_t)(unsafe.Pointer(&p[0])), C.int(len(p)), C.int(unit))
	runtime.KeepAlive(p)
	return int(n)
}
func packBGRNEON(dst, src []byte) {
	if len(src) == 0 {
		return
	}
	C.bk_pixels_pack((*C.uint8_t)(unsafe.Pointer(&dst[0])), (*C.uint8_t)(unsafe.Pointer(&src[0])), C.int(len(src)/4))
	runtime.KeepAlive(dst)
	runtime.KeepAlive(src)
}
