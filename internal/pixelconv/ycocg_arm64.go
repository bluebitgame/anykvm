//go:build cgo && arm64 && (darwin || linux || windows)

package pixelconv

func init() { selected = implementation{"NEON", convertNEON, deltaNEON} }
func convertNEON(dst, first, second []byte, fs, ss, w, h int, nv12 bool) {
	convertNative(0, dst, first, second, fs, ss, w, h, nv12)
}
func deltaNEON(dst, row, prev []byte) { deltaNative(0, dst, row, prev) }
