//go:build !darwin && (!linux || !cgo)

package capture

// ListScreens returns local displays when the platform exposes a native
// enumerator. Callers fall back to the unindexed screen selector when empty.
func ListScreens() []Screen { return nil }
