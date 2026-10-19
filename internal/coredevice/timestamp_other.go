//go:build !darwin || !cgo

package coredevice

import "time"

var reportClockOrigin = time.Now()

func reportTimestamp() uint64 { return uint64(time.Since(reportClockOrigin).Nanoseconds()) }
