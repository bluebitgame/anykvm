// Package viewer implements the optional local GUI, input forwarding, and
// bridge frame adapter. Build with the viewer tag to include SDL window support.
package viewer

import "errors"

// ErrTakenOver indicates that a remote client replaced the local viewer session.
// The application can continue serving that client without the GUI.
var ErrTakenOver = errors.New("viewer session taken over")
