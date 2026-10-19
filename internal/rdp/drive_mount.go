package rdp

import (
	"context"
	"log"
	"strings"
	"time"
	"unicode"
)

type driveVolume interface {
	Path() string
	Close() error
}

type driveMounter func(context.Context, string, string, bool) (driveVolume, error)

// Called after device replies have been sent. Native mounting makes WebDAV
// requests, so it must never block the RDP reader that completes those requests.
func (c *client) startDriveMount() {
	if !c.s.cfg.DriveRedirection.AutoMount || !driveMountSupported {
		return
	}
	d := &c.drives
	d.mu.Lock()
	if d.closed || d.mountCancel != nil || d.url == "" || len(d.devices) == 0 {
		d.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.mountCancel = cancel
	endpoint, name, mount := d.url, driveVolumeName(d.clientName), d.mounter
	if mount == nil {
		mount = mountDriveVolume
	}
	readOnly := c.s.cfg.DriveRedirection.Permissions&2 == 0
	c.s.wg.Add(1)
	d.mu.Unlock()
	go func() {
		defer c.s.wg.Done()
		defer cancel()
		mountCtx, stop := context.WithTimeout(ctx, 30*time.Second)
		volume, err := mount(mountCtx, endpoint, name, readOnly)
		stop()
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("rdp: cannot automatically mount client drives: %v", err)
			}
			return
		}
		if ctx.Err() == nil {
			log.Printf("rdp: client drives mounted at %s", volume.Path())
		}
		<-ctx.Done()
		if err := volume.Close(); err != nil {
			log.Printf("rdp: cannot unmount client drives at %s: %v", volume.Path(), err)
		} else {
			log.Printf("rdp: client drives unmounted")
		}
	}()
}

// A client-provided label is display data, never a filesystem path.
func driveVolumeName(name string) string {
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || unicode.IsControl(r) || r == unicode.ReplacementChar {
			return '-'
		}
		return r
	}, name))
	var label strings.Builder
	for _, r := range name {
		if label.Len()+len(string(r)) > 200 {
			break
		}
		label.WriteRune(r)
	}
	name = strings.Trim(label.String(), ". -")
	if name == "" {
		return "AnyKVM"
	}
	return name
}
