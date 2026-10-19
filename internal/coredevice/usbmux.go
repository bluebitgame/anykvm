package coredevice

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/danielpaulus/go-ios/ios"
)

// Keep the SDK's usbmux/Lockdown codecs, but own the sockets so every startup
// operation has the caller's deadline and can be interrupted by Close.
func dialMux(ctx context.Context) (*ios.UsbMuxConnection, net.Conn, func() bool, error) {
	network, address, ok := strings.Cut(ios.GetUsbmuxdSocket(), "://")
	if !ok || (network != "unix" && network != "tcp") {
		return nil, nil, nil, fmt.Errorf("invalid usbmux socket address")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
	if err != nil {
		return nil, nil, nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	if d, ok := ctx.Deadline(); ok {
		conn.SetDeadline(d)
	}
	return ios.NewUsbMuxConnection(ios.NewDeviceConnectionWithConn(conn)), conn, stop, nil
}
func listUSB(ctx context.Context) (ios.DeviceList, error) {
	mux, conn, stop, err := dialMux(ctx)
	if err != nil {
		return ios.DeviceList{}, err
	}
	defer stop()
	defer conn.Close()
	return mux.ListDevices()
}

type usbService struct {
	ios.DeviceConnectionInterface
	cancel func() bool
}

func (s *usbService) Close() error { s.cancel(); return s.DeviceConnectionInterface.Close() }
func (s *usbService) live()        { s.cancel(); s.Conn().SetDeadline(time.Time{}) }
func connectUSBService(ctx context.Context, device ios.DeviceEntry, name string) (*usbService, error) {
	mux, conn, stop, err := dialMux(ctx)
	if err != nil {
		return nil, err
	}
	defer stop()
	defer conn.Close()
	pair, err := mux.ReadPair(device.Properties.SerialNumber)
	if err != nil {
		return nil, fmt.Errorf("read host pairing record: %w", err)
	}
	lockdown, err := mux.ConnectLockdown(device.DeviceID)
	if err != nil {
		return nil, err
	}
	defer lockdown.Close()
	if _, err = lockdown.StartSession(pair); err != nil {
		return nil, fmt.Errorf("start trusted Lockdown session: %w", err)
	}
	response, err := lockdown.StartService(name)
	if err != nil {
		return nil, err
	}
	if response.Port == 0 {
		return nil, fmt.Errorf("Lockdown returned a zero service port")
	}
	serviceMux, serviceConn, serviceStop, err := dialMux(ctx)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			serviceStop()
			serviceConn.Close()
		}
	}()
	if err = serviceMux.Connect(device.DeviceID, response.Port); err != nil {
		return nil, err
	}
	dc := serviceMux.ReleaseDeviceConnection()
	if response.EnableServiceSSL {
		if err = dc.EnableSessionSsl(pair); err != nil {
			return nil, err
		}
	}
	success = true
	return &usbService{DeviceConnectionInterface: dc, cancel: serviceStop}, nil
}
