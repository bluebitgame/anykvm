//go:build darwin && cgo

package gamestream

/*
#include <pthread.h>
#include <pthread/qos.h>

static unsigned int bk_input_qos(int *relative) {
 qos_class_t qos = QOS_CLASS_UNSPECIFIED;
 if (pthread_get_qos_class_np(pthread_self(), &qos, relative)) return QOS_CLASS_UNSPECIFIED;
 return qos;
}
static int bk_set_input_qos(unsigned int qos, int relative) {
 return pthread_set_qos_class_self_np((qos_class_t)qos, relative);
}
*/
import "C"

import (
	"runtime"

	"anykvm/internal/logging"
)

// QoS belongs to an OS thread, not a goroutine. Pin the control/input worker
// for its lifetime and restore the thread before returning it to Go's pool.
// Sunshine uses this same QoS class for its latency-sensitive control thread.
func prioritizeInputThread() func() {
	runtime.LockOSThread()
	var relative C.int
	previous := C.bk_input_qos(&relative)
	if previous == C.QOS_CLASS_UNSPECIFIED {
		previous, relative = C.QOS_CLASS_DEFAULT, 0
	}
	if code := C.bk_set_input_qos(C.QOS_CLASS_USER_INTERACTIVE, 0); code != 0 {
		logging.Infof("gamestream: could not prioritize input thread: %d", int(code))
	}
	return func() {
		if code := C.bk_set_input_qos(previous, relative); code != 0 {
			// Keep this thread locked; Go discards it when the worker exits.
			logging.Infof("gamestream: could not restore input thread priority: %d", int(code))
			return
		}
		runtime.UnlockOSThread()
	}
}
