//go:build darwin && cgo

package coredevice

/*
#include <mach/mach_time.h>
*/
import "C"

// HID's timestamp field carries the low 48 bits of the host Mach clock.
func reportTimestamp() uint64 { return uint64(C.mach_absolute_time()) }
