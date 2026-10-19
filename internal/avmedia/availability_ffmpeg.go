//go:build ffmpeg && cgo

package avmedia

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// Enabled reports compile-time inclusion, not runtime library availability.
const Enabled = true

var loadOnce sync.Once
var loadError error

// Available loads one consistent FFmpeg 9 library set. ANYKVM_FFMPEG_LIBDIR is
// useful for development; released apps place libraries beside the executable.
func Available() error {
	loadOnce.Do(func() {
		dirs := []string{}
		if dir := os.Getenv("ANYKVM_FFMPEG_LIBDIR"); dir != "" {
			if absolute, err := filepath.Abs(dir); err == nil {
				dirs = append(dirs, absolute)
			}
		}
		if exe, err := os.Executable(); err == nil {
			if p, e := filepath.EvalSymlinks(exe); e == nil {
				exe = p
			}
			dirs = append(dirs, filepath.Join(filepath.Dir(exe), "lib"), filepath.Dir(exe))
		}
		if runtime.GOOS == "darwin" {
			dirs = append(dirs, "/opt/homebrew/opt/ffmpeg@9/lib", "/usr/local/opt/ffmpeg@9/lib",
				"/opt/homebrew/opt/ffmpeg/lib", "/usr/local/opt/ffmpeg/lib")
		}
		dirs = append(dirs, "")
		loadError = loadLibraries(dirs)
	})
	return loadError
}
