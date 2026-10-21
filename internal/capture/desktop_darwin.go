//go:build darwin

package capture

/*
#cgo LDFLAGS: -framework CoreGraphics -framework CoreFoundation
#include "desktop_darwin.h"
*/
import "C"

import (
	"fmt"
	"time"
	"unsafe"

	"anykvm/internal/logging"
)

func modeFromNative(m C.bk_display_mode, index int) displayMode {
	return displayMode{width: int(m.width), height: int(m.height), pixelWidth: int(m.pixel_width), pixelHeight: int(m.pixel_height), refreshRate: float64(m.refresh_rate), index: index}
}

func (d *screenDevice) SetDesktopSize(size DesktopSize) (DesktopSize, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if size.Width < 2 || size.Height < 2 || size.Width > 8192 || size.Height > 8192 || size.RefreshRate < 0 || size.RefreshRate > 240 || size.ScalePercent < 0 || (size.ScalePercent > 0 && (size.ScalePercent < 100 || size.ScalePercent > 500)) {
		return DesktopSize{}, fmt.Errorf("invalid desktop size %dx%d", size.Width, size.Height)
	}
	size = size.WithScaleProtection()
	requestChanged := d.desktopRequest == nil || *d.desktopRequest != size
	d.desktopRequest = &size
	current, modes, err := d.displayModesLocked()
	if err != nil {
		return DesktopSize{}, err
	}
	mode, ok := nearestDisplayMode(modes, size, current)
	if !ok {
		if size.Height < 1080 {
			return DesktopSize{}, fmt.Errorf("desktop: no usable 1x mode with sufficient comparison edge for %dx%d (requested height below 1080)", size.Width, size.Height)
		}
		return DesktopSize{}, fmt.Errorf("desktop: no usable display mode with sufficient comparison edge for %dx%d", size.Width, size.Height)
	}
	var message [512]C.char
	switchStarted := time.Now()
	if C.bk_display_set(d.display, C.int(mode.index), &message[0], C.int(len(message))) == 0 {
		return DesktopSize{}, fmt.Errorf("desktop: %s", C.GoString(&message[0]))
	}
	var native C.bk_display_mode
	if C.bk_display_current(d.display, &native) == 0 {
		return DesktopSize{}, fmt.Errorf("desktop: cannot verify selected display mode")
	}
	if int(native.width) != mode.width || int(native.height) != mode.height || int(native.pixel_width) != mode.pixelWidth || int(native.pixel_height) != mode.pixelHeight {
		return DesktopSize{}, fmt.Errorf("desktop: selected mode was not applied: logical=%dx%d pixels=%dx%d, expected logical=%dx%d pixels=%dx%d", int(native.width), int(native.height), int(native.pixel_width), int(native.pixel_height), mode.width, mode.height, mode.pixelWidth, mode.pixelHeight)
	}
	actual := DesktopSize{Width: int(native.pixel_width), Height: int(native.pixel_height), ScalePercent: int(native.pixel_width) * 100 / int(native.width)}
	if actual.Width != current.pixelWidth || actual.Height != current.pixelHeight || int(native.width) != current.width || int(native.height) != current.height || float64(native.refresh_rate) != current.refreshRate || (size.ScalePercent > 0 && requestChanged) {
		fit := min(float64(size.Width)/float64(native.pixel_width), float64(size.Height)/float64(native.pixel_height))
		effectiveScale := fit * float64(native.pixel_width) / float64(native.width) * 100
		logging.Infof("capture: screen:%d desktop requested=%dx%d@%d scale=%d%% selected=%dx%d@%.3f logical=%dx%d native_scale=%d%% effective_scale=%.1f%% mode_switch=%.1fms", d.index, size.Width, size.Height, size.RefreshRate, size.ScalePercent, actual.Width, actual.Height, float64(native.refresh_rate), int(native.width), int(native.height), actual.ScalePercent, effectiveScale, float64(time.Since(switchStarted))/float64(time.Millisecond))
	}
	return actual, nil
}

// Caller holds mu. The first read retains the original mode for restoration.
func (d *screenDevice) displayModesLocked() (displayMode, []displayMode, error) {
	var message [512]C.char
	if d.display == nil {
		d.display = C.bk_display_open(C.int(d.index), &message[0], C.int(len(message)))
		if d.display == nil {
			return displayMode{}, nil, fmt.Errorf("desktop: %s", C.GoString(&message[0]))
		}
	}
	var native C.bk_display_mode
	if C.bk_display_current(d.display, &native) == 0 {
		return displayMode{}, nil, fmt.Errorf("desktop: current display mode unavailable")
	}
	current := modeFromNative(native, -1)
	var modes []displayMode
	for i, count := 0, int(C.bk_display_modes(d.display)); i < count; i++ {
		if C.bk_display_mode_at(d.display, C.int(i), &native) != 0 && native.usable != 0 {
			modes = append(modes, modeFromNative(native, i))
		}
	}
	return current, modes, nil
}

func (d *screenDevice) DesktopDisplayID() uint32 {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.display == nil {
		return uint32(C.bk_display_index_id(C.int(d.index)))
	}
	return uint32(C.bk_display_id(d.display))
}

func (d *screenDevice) RestoreDesktopSize() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.desktopRequest = nil
	if d.display == nil {
		return nil
	}
	var message [512]C.char
	if C.bk_display_restore(d.display, &message[0], C.int(len(message))) == 0 {
		return fmt.Errorf("desktop: %s", C.GoString(&message[0]))
	}
	C.bk_display_free(d.display)
	d.display = nil
	return nil
}

func screenDisplayID(handle unsafe.Pointer) C.uint32_t {
	if handle == nil {
		return 0
	}
	return C.bk_display_id(handle)
}
