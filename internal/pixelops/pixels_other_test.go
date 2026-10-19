//go:build !amd64

package pixelops

func testImplementations() []implementation {
	if selected.name != scalar.name {
		return []implementation{scalar, selected}
	}
	return []implementation{scalar}
}
