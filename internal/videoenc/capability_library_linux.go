//go:build linux && cgo && ffmpeg

package videoenc

/*
#include <stdlib.h>
int bk_linux_nvenc_supports(int);
int bk_linux_vaapi_supports(const char *, int);
*/
import "C"

import (
	"anykvm/internal/avmedia"
	"unsafe"
)

func librarySupportsCodec(codec string) bool {
	id := C.int(1)
	if codec == "av1" {
		id = 2
	} else if codec != "hevc" {
		return false
	}
	nvidia, devices := linuxH264Hardware("/sys", "/dev/dri")
	if nvidia && avmedia.HasEncoder(codec+"_nvenc") && C.bk_linux_nvenc_supports(id) != 0 {
		return true
	}
	if avmedia.HasEncoder(codec + "_vaapi") {
		for _, device := range devices {
			p := C.CString(device)
			supported := C.bk_linux_vaapi_supports(p, id) != 0
			C.free(unsafe.Pointer(p))
			if supported {
				return true
			}
		}
	}
	return false
}
