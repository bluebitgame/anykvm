package hid

// Use nearest-point rounding. Truncating here and in the protocol mapper
// shifts even a same-size desktop's pointer one pixel up/left.
func normalizedDisplayPoint(value, origin, extent int) int {
	return origin + (min(32767, max(0, value))*max(0, extent-1)+32767/2)/32767
}
