//go:build darwin

package capture

/*
#include <stdint.h>
void bk_screen_set_frame_ready(void *screen, void (*notify)(uintptr_t), uintptr_t context);
extern void bk_screen_frame_ready(uintptr_t context);
static inline void bk_screen_attach_frame_ready(void *screen, uintptr_t context) {
    bk_screen_set_frame_ready(screen, bk_screen_frame_ready, context);
}
*/
import "C"

import "runtime/cgo"

//export bk_screen_frame_ready
func bk_screen_frame_ready(context C.uintptr_t) {
	ready := cgo.Handle(context).Value().(chan struct{})
	select {
	case ready <- struct{}{}:
	default:
	}
}

func (d *screenDevice) CaptureReady() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ready
}

// Caller holds mu. Registration also signals any output produced during Open.
func (d *screenDevice) attachFrameReadyLocked() {
	if d.ready == nil {
		d.ready = make(chan struct{}, 1)
	}
	handle := cgo.NewHandle(d.ready)
	d.notification = uintptr(handle)
	C.bk_screen_attach_frame_ready(d.session, C.uintptr_t(handle))
}

func (d *screenDevice) detachFrameReadyLocked() {
	if d.notification != 0 {
		// The native lock serializes detachment with callbacks, so no callback
		// can reference this handle after the setter returns.
		C.bk_screen_set_frame_ready(d.session, nil, 0)
		cgo.Handle(d.notification).Delete()
		d.notification = 0
	}
}
