//go:build windows && !ffmpeg

package gamestream

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var windowsOpus struct {
	sync.Mutex
	api *opusFunctions
}

func newNativeOpusEncoder(duration int) (nativeOpusEncoder, error) {
	windowsOpus.Lock()
	api := windowsOpus.api
	if api == nil {
		var directories []string
		if directory := os.Getenv("ANYKVM_OPUS_LIBDIR"); directory != "" {
			directories = append(directories, directory)
		}
		if executable, err := os.Executable(); err == nil {
			directory := filepath.Dir(executable)
			directories = append(directories, filepath.Join(directory, "lib"), directory)
		}
		var err error
		api, err = loadWindowsOpus(directories)
		if err != nil {
			windowsOpus.Unlock()
			return nil, err
		}
		// Keep successful DLLs loaded for the process lifetime. Failed loads are
		// retried for the next session, allowing a missing DLL to be installed.
		windowsOpus.api = api
	}
	windowsOpus.Unlock()
	return newDLLOpusEncoder(duration, api)
}

func loadWindowsOpus(directories []string) (*opusFunctions, error) {
	var failures []error
	for _, directory := range directories {
		path, err := filepath.Abs(filepath.Join(directory, "opus.dll"))
		if err != nil {
			failures = append(failures, err)
			continue
		}
		// Search dependencies beside this DLL and in system locations, never in
		// the current working directory or PATH.
		handle, err := windows.LoadLibraryEx(path, 0, windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR|windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", path, err))
			continue
		}
		var procedures [4]uintptr
		for i, name := range [...]string{"opus_encoder_create", "opus_encoder_ctl", "opus_encode", "opus_encoder_destroy"} {
			procedures[i], err = windows.GetProcAddress(handle, name)
			if err != nil {
				failures = append(failures, fmt.Errorf("%s: %s: %w", path, name, err))
				break
			}
		}
		if err != nil {
			windows.FreeLibrary(handle)
			continue
		}
		return &opusFunctions{
			create: func(rate, channels, application int) (uintptr, int32) {
				var code int32
				encoder, _, _ := syscall.SyscallN(procedures[0], uintptr(rate), uintptr(channels), uintptr(application), uintptr(unsafe.Pointer(&code)))
				runtime.KeepAlive(&code)
				return encoder, code
			},
			control: func(encoder uintptr, request, value int) int32 {
				// All arguments are integers: the variadic C ABI is compatible
				// with SyscallN on both Windows AMD64 and ARM64.
				result, _, _ := syscall.SyscallN(procedures[1], encoder, uintptr(request), uintptr(value))
				return int32(result)
			},
			encode: func(encoder uintptr, pcm []int16, samples int, packet []byte) int32 {
				result, _, _ := syscall.SyscallN(procedures[2], encoder, uintptr(unsafe.Pointer(&pcm[0])), uintptr(samples), uintptr(unsafe.Pointer(&packet[0])), uintptr(len(packet)))
				runtime.KeepAlive(pcm)
				runtime.KeepAlive(packet)
				return int32(result)
			},
			destroy: func(encoder uintptr) { syscall.SyscallN(procedures[3], encoder) },
		}, nil
	}
	return nil, fmt.Errorf("Opus DLL unavailable; install a matching opus.dll in lib beside anykvm.exe or set ANYKVM_OPUS_LIBDIR: %w", errors.Join(failures...))
}
