//go:build linux && cgo

package capture

// #include <stdlib.h>
// #include "native_v4l2_linux.h"
import "C"

import (
	"fmt"
	"path/filepath"
	"strings"
	"unsafe"
)

var linuxModes deviceModeCache

func ListVideoModes(device string) ([]VideoMode, error) {
	if strings.TrimSpace(device) == "" || !filepath.IsAbs(device) || IsScreenDevice(device) {
		return nil, fmt.Errorf("V4L2 mode discovery requires a local absolute device path")
	}
	return linuxModes.list(device, func() ([]VideoMode, error) {
		name := C.CString(device)
		defer C.free(unsafe.Pointer(name))
		var msg [512]C.char
		data := C.bk_v4l_modes(name, &msg[0], C.size_t(len(msg)))
		if data == nil {
			return nil, fmt.Errorf("V4L2 modes %q: %s", device, C.GoString(&msg[0]))
		}
		defer C.free(unsafe.Pointer(data))
		return decodeDeviceVideoModes([]byte(C.GoString(data)))
	})
}
func ListNativeVideoDevices(bool) []string {
	paths, _ := filepath.Glob("/dev/video*")
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		p := C.CString(path)
		ok := C.bk_v4l_candidate(p) != 0
		C.free(unsafe.Pointer(p))
		if ok {
			result = append(result, path)
		}
	}
	return result
}
