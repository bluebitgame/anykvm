package rdp

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type testDriveVolume struct{ closed chan struct{} }

func (v *testDriveVolume) Path() string { return "/private/test-only" }
func (v *testDriveVolume) Close() error { close(v.closed); return nil }

func TestDriveMountLifecycle(t *testing.T) {
	if !driveMountSupported {
		t.Skip("automatic mounting requires macOS or Linux")
	}
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "mounted", true: "disconnect_during_mount"}[late], func(t *testing.T) {
			c := &client{s: &Server{cfg: Config{Username: "test", Password: "secret", DriveRedirection: DriveRedirectionOptions{Permissions: 1, AutoMount: true}}}}
			c.drives.url = "http://127.0.0.1:12345/"
			c.drives.clientName = "MY-PC"
			c.drives.devices = map[uint32]*redirectedDrive{1: {id: 1, name: "c"}}
			entered, release := make(chan struct{}), make(chan struct{})
			volume := &testDriveVolume{closed: make(chan struct{})}
			var calls atomic.Int32
			c.drives.mounter = func(ctx context.Context, endpoint, name string, ro bool) (driveVolume, error) {
				calls.Add(1)
				if endpoint != c.drives.url || name != "MY-PC" || !ro {
					t.Error("mount settings not preserved")
				}
				close(entered)
				if late {
					<-ctx.Done()
				} else {
					<-release
				}
				// Even a mount completing after cancellation must be unmounted.
				return volume, nil
			}
			c.startDriveMount()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("mount did not start")
			}
			c.startDriveMount()
			if !late {
				close(release)
			}
			c.closeDrives()
			select {
			case <-volume.closed:
			case <-time.After(time.Second):
				t.Fatal("owned volume not unmounted")
			}
			c.s.wg.Wait()
			c.startDriveMount()
			if calls.Load() != 1 {
				t.Fatal("mounted more than once")
			}
		})
	}
}

func TestDriveMountFailure(t *testing.T) {
	if !driveMountSupported {
		t.Skip("automatic mounting requires macOS or Linux")
	}
	c := &client{s: &Server{cfg: Config{DriveRedirection: DriveRedirectionOptions{Permissions: 3, AutoMount: true}}}}
	c.drives.url = "http://127.0.0.1:12345/"
	c.drives.devices = map[uint32]*redirectedDrive{1: {id: 1}}
	var calls atomic.Int32
	c.drives.mounter = func(context.Context, string, string, bool) (driveVolume, error) {
		calls.Add(1)
		return nil, errors.New("test mount failure")
	}
	c.s.cfg.DriveRedirection.AutoMount = false
	c.startDriveMount()
	if calls.Load() != 0 {
		t.Fatal("disabled mount started")
	}
	c.s.cfg.DriveRedirection.AutoMount = true
	c.startDriveMount()
	c.s.wg.Wait()
	c.startDriveMount()
	c.s.wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("failed mount retried without a new session")
	}
	c.closeDrives()
}

func TestDriveVolumeName(t *testing.T) {
	for input, want := range map[string]string{"": "AnyKVM", "..": "AnyKVM", "  MY-PC  ": "MY-PC", "小明的电脑": "小明的电脑", "../../a:b\\c\x00": "a-b-c", "---": "AnyKVM"} {
		if got := driveVolumeName(input); got != want {
			t.Errorf("%q -> %q, want %q", input, got, want)
		}
	}
	name := driveVolumeName(strings.Repeat("中", 300))
	if len(name) > 200 {
		t.Fatal("volume label is too long")
	}
}
