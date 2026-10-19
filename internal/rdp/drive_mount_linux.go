//go:build linux

package rdp

import (
	"context"
	"os"
)

const driveMountSupported = true

func mountDriveVolume(ctx context.Context, endpoint, name string, readOnly bool) (driveVolume, error) {
	return mountLinuxDrive(ctx, endpoint, name, readOnly, linuxDriveMountRoot(os.Getenv))
}
