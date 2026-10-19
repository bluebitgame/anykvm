//go:build linux

package rdp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDriveNativeLinuxAutoMount(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_LINUX_MOUNT") != "1" {
		t.Skip("set ANYKVM_TEST_LINUX_MOUNT=1 with rclone and FUSE available")
	}
	for _, ro := range []bool{false, true} {
		t.Run(map[bool]string{false: "read_write", true: "read_only"}[ro], func(t *testing.T) {
			root := t.TempDir()
			mounted := make(chan driveVolume, 1)
			permissions := uint8(3)
			if ro {
				permissions = 1
			}
			p := newDrivePeerWithMount(t, permissions, true, func(ctx context.Context, endpoint, name string, readOnly bool) (driveVolume, error) {
				v, err := mountLinuxDrive(ctx, endpoint, name, readOnly, root)
				if err == nil {
					mounted <- v
				}
				return v, err
			})
			var v driveVolume
			select {
			case v = <-mounted:
			case <-time.After(35 * time.Second):
				t.Fatal("Linux automatic mount did not become ready")
			}
			if filepath.Base(v.Path()) != "test" {
				t.Fatal("client computer name not used")
			}
			data, err := os.ReadFile(filepath.Join(v.Path(), "c", "你好.txt"))
			if err != nil || string(data) != "hello client" {
				t.Fatalf("mounted read: %q %v", data, err)
			}
			err = os.WriteFile(filepath.Join(v.Path(), "c", "linux-write.txt"), []byte("Linux native mount"), 0600)
			if ro && err == nil {
				t.Fatal("read-only mount accepted a write")
			}
			if !ro {
				if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(filepath.Join(p.root, "linux-write.txt"))
				if err != nil || string(data) != "Linux native mount" {
					t.Fatalf("mounted write: %q %v", data, err)
				}
			}
			p.c.closeDrives()
			deadline := time.Now().Add(8 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := os.Stat(v.Path()); os.IsNotExist(err) {
					return
				}
				time.Sleep(25 * time.Millisecond)
			}
			t.Fatal("automatic mount survived disconnect")
		})
	}
}

func TestDriveNativeLinuxHeadlessMount(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_LINUX_MOUNT") != "1" || os.Geteuid() != 0 {
		t.Skip("headless /mnt integration requires root and ANYKVM_TEST_LINUX_MOUNT=1")
	}
	for _, key := range []string{"XDG_SESSION_TYPE", "XDG_RUNTIME_DIR", "DISPLAY", "WAYLAND_DISPLAY", "SSH_CONNECTION", "SSH_TTY"} {
		t.Setenv(key, "")
	}
	mounted := make(chan driveVolume, 1)
	p := newDrivePeerWithMount(t, 3, true, func(ctx context.Context, endpoint, name string, ro bool) (driveVolume, error) {
		v, err := mountDriveVolume(ctx, endpoint, name, ro)
		if err == nil {
			mounted <- v
		}
		return v, err
	})
	var v driveVolume
	select {
	case v = <-mounted:
	case <-time.After(35 * time.Second):
		t.Fatal("headless mount did not become ready")
	}
	if filepath.Dir(v.Path()) != "/mnt" {
		t.Fatalf("headless mount is not under /mnt: %s", v.Path())
	}
	if _, err := os.ReadFile(filepath.Join(v.Path(), "c", "你好.txt")); err != nil {
		t.Fatal(err)
	}
	p.c.closeDrives()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(v.Path()); os.IsNotExist(err) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("headless mountpoint survived disconnect")
}
