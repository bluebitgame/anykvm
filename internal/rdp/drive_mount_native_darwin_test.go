//go:build darwin

package rdp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDriveNativeAutoMount(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_WEBDAV_NATIVE") != "1" {
		t.Skip("set ANYKVM_TEST_WEBDAV_NATIVE=1")
	}
	for _, ro := range []bool{false, true} {
		t.Run(map[bool]string{false: "read_write", true: "read_only"}[ro], func(t *testing.T) {
			p := newDrivePeer(t, 3)
			p.c.drives.mu.Lock()
			endpoint := p.c.drives.url
			name := p.c.drives.clientName
			p.c.drives.mu.Unlock()
			if name != "test" {
				t.Fatalf("reported client name not decoded: %q", name)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			v, err := mountDriveVolume(ctx, endpoint, "AnyKVM-test-client", ro)
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			if filepath.Base(v.Path()) != "AnyKVM-test-client" {
				t.Fatal("wrong mount label")
			}
			data, err := os.ReadFile(filepath.Join(v.Path(), "c", "你好.txt"))
			if err != nil || string(data) != "hello client" {
				t.Fatalf("mounted file read: %q %v", data, err)
			}
			err = os.WriteFile(filepath.Join(v.Path(), "c", "mount-write.txt"), []byte("native mount write"), 0600)
			if ro && err == nil {
				t.Fatal("read-only mount accepted write")
			}
			if !ro {
				if err != nil {
					t.Fatal(err)
				}
				data, err = os.ReadFile(filepath.Join(p.root, "mount-write.txt"))
				if err != nil || string(data) != "native mount write" {
					t.Fatalf("mounted write did not reach client: %q %v", data, err)
				}
			}
			p.c.closeDrives()
			if err := v.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(v.Path()); !os.IsNotExist(err) {
				t.Fatalf("mountpoint survived disconnect: %v", err)
			}
		})
	}
}

func TestDriveNativeAutoMountSession(t *testing.T) {
	if os.Getenv("ANYKVM_TEST_WEBDAV_NATIVE") != "1" {
		t.Skip("set ANYKVM_TEST_WEBDAV_NATIVE=1")
	}
	mounted := make(chan driveVolume, 1)
	p := newDrivePeerWithMount(t, 3, true, func(ctx context.Context, endpoint, name string, ro bool) (driveVolume, error) {
		v, err := mountDriveVolume(ctx, endpoint, name, ro)
		if err == nil {
			mounted <- v
		}
		return v, err
	})
	var volume driveVolume
	select {
	case volume = <-mounted:
	case <-time.After(35 * time.Second):
		t.Fatal("session did not automatically mount")
	}
	if filepath.Base(volume.Path()) != "test" {
		t.Fatal("client machine name not used")
	}
	data, err := os.ReadFile(filepath.Join(volume.Path(), "c", "你好.txt"))
	if err != nil || string(data) != "hello client" {
		t.Fatal("automatic mount cannot read client", err)
	}
	parent := filepath.Dir(volume.Path())
	p.c.closeDrives()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(parent); os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("session-owned mount directory survived disconnect")
}
