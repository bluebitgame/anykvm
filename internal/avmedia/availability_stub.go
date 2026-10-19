//go:build !ffmpeg || !cgo

package avmedia

import "fmt"

// Enabled reports compile-time inclusion, not runtime library availability.
const Enabled = false

var disabledError = fmt.Errorf("%w: support disabled; rebuild with -tags ffmpeg and CGO_ENABLED=1", ErrUnavailable)

// Available never searches for or loads libraries in a native-only build.
func Available() error { return disabledError }
