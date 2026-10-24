// Package logging controls optional runtime logs while using the standard
// logger's output, timestamps and synchronization.
package logging

import (
	"fmt"
	"log"
	"sync/atomic"
)

var verbosity atomic.Int32

// DebugEnabled lets native backends gate diagnostics at the same log level.
func DebugEnabled() bool { return verbosity.Load() >= 2 }

// Configure enables connection logs with info, and all logs with debug.
// Debug takes precedence when both flags are supplied.
func Configure(info, debug bool) {
	var level int32
	if debug {
		level = 2
	} else if info {
		level = 1
	}
	verbosity.Store(level)
}

// Basicf prints startup information at every verbosity level.
func Basicf(format string, args ...any) {
	_ = log.Output(2, fmt.Sprintf(format, args...))
}

// Infof prints connection and runtime logs with -info or -debug.
func Infof(format string, args ...any) {
	if verbosity.Load() >= 1 {
		_ = log.Output(2, fmt.Sprintf(format, args...))
	}
}

// Info is the unformatted counterpart of Infof.
func Info(args ...any) {
	if verbosity.Load() >= 1 {
		_ = log.Output(2, fmt.Sprint(args...))
	}
}

// Debugf prints periodic statistics and diagnostics only with -debug.
func Debugf(format string, args ...any) {
	if verbosity.Load() >= 2 {
		_ = log.Output(2, fmt.Sprintf(format, args...))
	}
}
