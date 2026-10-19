//go:build windows

package service

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

var serviceUser32 = windows.NewLazySystemDLL("user32.dll")
var serviceOpenInputDesktop = serviceUser32.NewProc("OpenInputDesktop")
var serviceCloseDesktop = serviceUser32.NewProc("CloseDesktop")
var serviceGetUserObjectInformation = serviceUser32.NewProc("GetUserObjectInformationW")

func activeWindowsDesktop() (string, error) {
	handle, _, err := serviceOpenInputDesktop.Call(0, 0, 1) // DESKTOP_READOBJECTS
	if handle == 0 {
		return "", fmt.Errorf("OpenInputDesktop: %w", err)
	}
	defer serviceCloseDesktop.Call(handle)
	buffer := make([]uint16, 256)
	var size uint32
	ok, _, err := serviceGetUserObjectInformation.Call(handle, 2, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)*2), uintptr(unsafe.Pointer(&size))) // UOI_NAME
	if ok == 0 {
		return "", fmt.Errorf("GetUserObjectInformation: %w", err)
	}
	return windows.UTF16ToString(buffer), nil
}

func prepareWindowsWorker(opts *Options) error {
	stopName := os.Getenv(WorkerStopEnvironment)
	if stopName == "" {
		return nil
	} // A normal console launch keeps its original behavior.
	desktop := os.Getenv(workerDesktopEnvironment)
	if serviceDesktopExit(desktop) == 0 {
		return fmt.Errorf("invalid service worker desktop %q", desktop)
	}
	ptr, err := windows.UTF16PtrFromString(stopName)
	if err != nil {
		return err
	}
	event, err := windows.OpenEvent(windows.SYNCHRONIZE, false, ptr)
	if err != nil {
		return fmt.Errorf("open worker shutdown event: %w", err)
	}
	// The process must be created on the right desktop so FFmpeg and input
	// threads inherit it. Changing one Go thread's desktop is insufficient.
	if current, err := activeWindowsDesktop(); err == nil && !strings.EqualFold(current, desktop) {
		if code := serviceDesktopExit(current); code != 0 {
			windows.CloseHandle(event)
			os.Exit(int(code))
		}
	}
	stop, done, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var exitCode atomic.Uint32
	opts.Stop = stop
	opts.Done = func() {
		close(done)
		<-finished
		windows.CloseHandle(event)
		if code := exitCode.Load(); code != 0 {
			os.Exit(int(code))
		}
	}
	go func() {
		defer close(finished)
		for {
			select {
			case <-done:
				return
			default:
			}
			result, err := windows.WaitForSingleObject(event, 1000)
			if err != nil {
				log.Printf("worker shutdown event: %v", err)
				close(stop)
				return
			}
			if result == windows.WAIT_OBJECT_0 {
				close(stop)
				return
			}
			current, err := activeWindowsDesktop()
			if err != nil || strings.EqualFold(current, desktop) {
				continue
			}
			if code := serviceDesktopExit(current); code != 0 {
				log.Printf("AnyKVM: input desktop changed to %s; restarting worker", current)
				exitCode.Store(code)
				close(stop)
				return
			}
		}
	}()
	return nil
}
