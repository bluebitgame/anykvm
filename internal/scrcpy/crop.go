package scrcpy

import (
	"fmt"
	"strconv"
	"strings"
)

// ValidateCrop accepts width:height:x:y in natural device coordinates.
// Empty captures the full display. Screen bounds are checked by scrcpy-server.
func ValidateCrop(value string) error {
	if value == "" {
		return nil
	}
	invalid := func() error {
		return fmt.Errorf("invalid crop %q (use width:height:x:y with positive dimensions and non-negative offsets)", value)
	}
	parts := strings.Split(value, ":")
	if len(parts) != 4 {
		return invalid()
	}
	var values [4]uint64
	for i, part := range parts {
		n, err := strconv.ParseUint(part, 10, 31)
		if err != nil {
			return invalid()
		}
		values[i] = n
	}
	if values[0] == 0 || values[1] == 0 {
		return invalid()
	}
	// Android's Rect stores the right/bottom coordinates as signed 32-bit ints.
	if values[0]+values[2] > 1<<31-1 || values[1]+values[3] > 1<<31-1 {
		return fmt.Errorf("invalid crop %q: rectangle coordinates exceed Android's integer range", value)
	}
	return nil
}
