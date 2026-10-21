//go:build windows && cgo

package videoenc

/*
#cgo windows LDFLAGS: -ld3d11 -ldxgi -ldxguid -ladvapi32 -lcfgmgr32
#include "vendor_windows.h"
#include "mediafoundation_windows.h"
*/
import "C"

import (
	"anykvm/internal/avmedia"
	"strings"
)

func platformSupportsCodec(codec string) bool {
	id := C.int(1)
	if codec == "av1" {
		id = 2
	} else if codec != "hevc" {
		return false
	}
	for _, name := range windowsHardwareCodecs(codec) {
		if avmedia.Enabled && !avmedia.HasEncoder(name) {
			continue
		}
		for vendor, suffix := range []string{"nvenc", "amf", "qsv"} {
			if strings.HasSuffix(name, "_"+suffix) && C.bk_vendor_supports(C.int(vendor), id) != 0 {
				return true
			}
		}
	}
	return C.bk_mf_supports(id) != 0
}
