package coredevice

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"time"

	"github.com/danielpaulus/go-ios/ios"
)

const springboardService = "com.apple.springboardservices"
const springboardShim = springboardService + ".shim.remote"

// SpringBoard reports the interface orientation, including the foreground app,
// rather than the accelerometer's physical device orientation.
func displayRotation(orientation int32, width, height int) int {
	// Some encoders already deliver an upright landscape-sized image. Do not
	// rotate it a second time when its dimensions match the interface.
	if width > height && (orientation == 3 || orientation == 4) {
		return 0
	}
	return interfaceRotation(orientation)
}

// Rotation from the fixed portrait touchscreen to the upright app interface.
// Unlike displayRotation, this applies even when AVFoundation rotates pixels.
func interfaceRotation(orientation int32) int {
	switch orientation {
	case 2:
		return 180
	case 3: // Home button to the right; DisplayService content points right.
		return 270
	case 4: // Home button to the left.
		return 90
	default:
		return 0
	}
}

func (s *session) startOrientationTracking() {
	s.client.orientation.Store(0)
	ctx, cancel := context.WithCancel(context.Background())
	s.orientationCancel = cancel
	s.orientationDone = make(chan struct{})
	go s.orientationLoop(ctx)
}

func (s *session) setOrientation(orientation int32) {
	if s.client.inputOnly {
		s.input.mu.Lock()
		s.input.orientation = orientation
		s.input.updateDisplayRotation()
		s.input.mu.Unlock()
	}
	if old := s.client.orientation.Swap(orientation); old != orientation {
		log.Printf("coredevice: interface orientation=%d", orientation)
	}
}

type orientationChannel struct {
	codec  ios.PlistCodecReadWriter
	conn   net.Conn
	closer io.Closer
	cancel func() bool
}

func (c *orientationChannel) Close() {
	c.cancel()
	c.closer.Close()
}

func openOrientationChannel(ctx context.Context, e *endpoint) (*orientationChannel, error) {
	if port := e.ports[springboardShim]; port != 0 {
		dialCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", net.JoinHostPort(e.address, strconv.Itoa(port)))
		if err != nil {
			return nil, err
		}
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if err := ios.RsdCheckin(conn); err != nil {
			stop()
			conn.Close()
			return nil, err
		}
		return &orientationChannel{codec: ios.NewPlistCodecReadWriter(conn, conn), conn: conn, closer: conn, cancel: stop}, nil
	}
	// A trusted USB connection also supplies SpringBoard when the developer
	// service list omits the shim. Match the active phone, never the first device.
	startup, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	devices, err := listUSB(startup)
	if err != nil {
		return nil, err
	}
	for _, device := range devices.DeviceList {
		if device.Properties.ConnectionType != "USB" || device.Properties.SerialNumber != e.udid {
			continue
		}
		service, err := connectUSBService(startup, device, springboardService)
		if err != nil {
			return nil, err
		}
		service.live()
		stop := context.AfterFunc(ctx, func() { service.Close() })
		return &orientationChannel{codec: ios.NewPlistCodecReadWriter(service.Reader(), service.Writer()), conn: service.Conn(), closer: service, cancel: stop}, nil
	}
	return nil, fmt.Errorf("SpringBoard orientation service is unavailable for the active phone")
}

func (c *orientationChannel) read() (int32, error) {
	_ = c.conn.SetDeadline(time.Now().Add(time.Second))
	if err := c.codec.Write(map[string]interface{}{"command": "getInterfaceOrientation"}); err != nil {
		return 0, err
	}
	var response map[string]interface{}
	if err := c.codec.Read(&response); err != nil {
		return 0, err
	}
	value := number(response["interfaceOrientation"])
	if value < 1 || value > 4 {
		return 0, fmt.Errorf("invalid interface orientation: %v", response["interfaceOrientation"])
	}
	return int32(value), nil
}

func (s *session) orientationLoop(ctx context.Context) {
	defer close(s.orientationDone)
	var channel *orientationChannel
	defer func() {
		if channel != nil {
			channel.Close()
		}
	}()
	warned := false
	for ctx.Err() == nil {
		var err error
		if channel == nil {
			channel, err = openOrientationChannel(ctx, s.endpoint)
		}
		if err == nil {
			var orientation int32
			orientation, err = channel.read()
			if err == nil && ctx.Err() == nil {
				s.setOrientation(orientation)
				warned = false
			}
		}
		delay := 250 * time.Millisecond
		if err != nil {
			if channel != nil {
				channel.Close()
				channel = nil
			}
			if !warned && ctx.Err() == nil {
				log.Printf("coredevice: automatic orientation unavailable: %v; will retry", err)
				warned = true
			}
			delay = 5 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}
