//go:build !windows

package hid

import "strings"

func platformDevicePath(path string) string {
	if path == "" || strings.HasPrefix(path, "/") {
		return path
	}
	return "/dev/" + path
}
