//go:build !darwin && !linux

package rdp

import (
	"context"
	"errors"
)

const driveMountSupported = false

func mountDriveVolume(context.Context, string, string, bool) (driveVolume, error) {
	return nil, errors.New("automatic drive mounting requires macOS or Linux")
}
