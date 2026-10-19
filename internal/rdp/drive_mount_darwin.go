//go:build darwin

package rdp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const driveMountSupported = true

type nativeDriveVolume struct{ path, parent string }

func (v *nativeDriveVolume) Path() string { return v.path }
func (v *nativeDriveVolume) Close() error {
	err := unix.Unmount(v.path, unix.MNT_FORCE)
	if err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOENT) {
		return fmt.Errorf("unmount: %w", err)
	}
	// Never recursively remove a mountpoint: a failed unmount must not delete
	// anything in the client's filesystem. These directories are session-owned.
	if err := os.Remove(v.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(v.parent); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func mountDriveVolume(ctx context.Context, endpoint, name string, readOnly bool) (driveVolume, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	parent, err := os.MkdirTemp("", "anykvm-rdp-mount-")
	if err != nil {
		return nil, err
	}
	name = driveVolumeName(name)
	v := &nativeDriveVolume{parent: parent, path: filepath.Join(parent, name)}
	if err := os.Mkdir(v.path, 0700); err != nil {
		_ = os.Remove(parent)
		return nil, err
	}
	// -S suppresses authentication and disconnect dialogs. No credentials are
	// required by the private loopback server. Set the Finder volume label explicitly.
	args := []string{"-S", "-a", "3", "-v", name}
	if readOnly {
		args = append(args, "-o", "rdonly")
	}
	args = append(args, endpoint, v.path)
	output, err := func() ([]byte, error) {
		// Apple's -a fd interface skips keychain lookups, which can otherwise
		// prompt or block even with -S. Supply five zero-length fields (user,
		// password, proxy user/password, certificate chain), with no credentials.
		// See apple-oss-distributions/webdavfs mount.tproj/webdav_agent.c.
		auth, err := os.CreateTemp(parent, ".mount-options-")
		if err != nil {
			return nil, err
		}
		defer auth.Close()
		defer os.Remove(auth.Name())
		if _, err := auth.Write(make([]byte, 20)); err != nil {
			return nil, err
		}
		cmd := exec.CommandContext(ctx, "/sbin/mount_webdav", args...)
		cmd.ExtraFiles = []*os.File{auth}
		cmd.WaitDelay = 2 * time.Second
		return cmd.CombinedOutput()
	}()
	if err == nil && ctx.Err() == nil {
		return v, nil
	}
	cleanupErr := v.Close()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		message := strings.TrimSpace(string(output))
		if len(message) > 512 {
			message = message[:512]
		}
		err = fmt.Errorf("mount_webdav: %w (%s)", err, message)
	}
	return nil, errors.Join(err, cleanupErr)
}
