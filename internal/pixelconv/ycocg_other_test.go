//go:build !cgo || !amd64 || !(darwin || linux || windows)

package pixelconv

func testImplementations() []implementation {
	if selected.name != scalar.name {
		return []implementation{scalar, selected}
	}
	return []implementation{scalar}
}
