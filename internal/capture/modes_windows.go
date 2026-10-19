//go:build windows && cgo

package capture

// #include <stdlib.h>
// #include "native_windows.h"
import "C"

import (
	"fmt"
	"strings"
	"unsafe"
)

var windowsModes deviceModeCache

// ListVideoModes enumerates Media Foundation native types without reading samples.
func ListVideoModes(device string) ([]VideoMode, error) {
	return windowsModes.list(device, func() ([]VideoMode, error) { return queryWindowsVideoModes(device) })
}

func queryWindowsVideoModes(device string) ([]VideoMode, error) {
	if strings.TrimSpace(device) == "" || IsScreenDevice(device) {
		return nil, fmt.Errorf("capture modes require a local video input device")
	}
	name := C.CString(device)
	defer C.free(unsafe.Pointer(name))
	var msg [512]C.char
	data := C.bk_win_modes(name, &msg[0], C.size_t(len(msg)))
	if data == nil {
		return nil, fmt.Errorf("Windows capture modes %q: %s", device, C.GoString(&msg[0]))
	}
	defer C.free(unsafe.Pointer(data))
	return decodeDeviceVideoModes([]byte(C.GoString(data)))
}
