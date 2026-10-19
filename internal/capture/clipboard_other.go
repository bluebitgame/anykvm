//go:build !darwin

package capture

// Other platforms can supply a native backend through rdp.Config.Clipboard.
func NewClipboard() Clipboard { return nil }
