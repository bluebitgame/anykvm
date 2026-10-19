//go:build viewer && darwin

package viewer

/*
#cgo LDFLAGS: -framework GameController -framework Foundation

typedef void (*anykvm_gc_notify_fn)(void);

extern int anykvm_gc_start(void);
extern void anykvm_gc_stop(void);
extern void anykvm_gc_resync(void);
extern void anykvm_gc_set_active(int on);
extern void anykvm_gc_set_notify(anykvm_gc_notify_fn fn);
extern int anykvm_gc_drain(float *dx, float *dy);
extern int anykvm_gc_mouse_count(void);
extern void anykvm_gc_go_notify(void);
*/
import "C"

var gcMouseWake chan struct{}

func platformStartRawMouse() int {
	return int(C.anykvm_gc_start())
}

func platformStopRawMouse() {
	C.anykvm_gc_set_notify(nil)
	C.anykvm_gc_stop()
}

func platformResyncRawMouse() {
	C.anykvm_gc_resync()
}

func platformGCMouseCount() int {
	return int(C.anykvm_gc_mouse_count())
}

func platformSetRawMouseActive(on bool) {
	v := C.int(0)
	if on {
		v = 1
	}
	C.anykvm_gc_set_active(v)
}

func platformDrainRawMouse() (dx, dy float32, ok bool) {
	var cdx, cdy C.float
	if C.anykvm_gc_drain(&cdx, &cdy) == 0 {
		return 0, 0, false
	}
	return float32(cdx), float32(cdy), true
}

func platformStartGCMouseRelay(wake chan struct{}) {
	gcMouseWake = wake
	C.anykvm_gc_set_notify(C.anykvm_gc_notify_fn(C.anykvm_gc_go_notify))
}

func platformStopGCMouseRelay() {
	C.anykvm_gc_set_notify(nil)
	gcMouseWake = nil
}

//export anykvm_gc_go_notify
func anykvm_gc_go_notify() {
	wake := gcMouseWake
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}
