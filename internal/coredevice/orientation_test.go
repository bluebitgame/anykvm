package coredevice

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/danielpaulus/go-ios/ios"
)

func TestDisplayRotation(t *testing.T) {
	for _, tc := range []struct {
		orientation         int32
		width, height, want int
	}{
		{0, 1328, 2896, 0}, {1, 1328, 2896, 0}, {2, 1328, 2896, 180},
		{3, 1328, 2896, 270}, {4, 1328, 2896, 90},
		{3, 2896, 1328, 0}, {4, 2896, 1328, 0},
	} {
		if got := displayRotation(tc.orientation, tc.width, tc.height); got != tc.want {
			t.Fatalf("orientation=%d size=%dx%d rotation=%d want=%d", tc.orientation, tc.width, tc.height, got, tc.want)
		}
	}
}

func TestSpringBoardOrientationQueryAndValidation(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	channel := &orientationChannel{codec: ios.NewPlistCodecReadWriter(client, client), conn: client, closer: client, cancel: context.AfterFunc(ctx, func() { client.Close() })}
	defer channel.Close()
	finished := make(chan error, 1)
	go func() {
		codec := ios.NewPlistCodecReadWriter(server, server)
		for _, value := range []uint64{3, 4, 0, 99} {
			var request map[string]interface{}
			if err := codec.Read(&request); err != nil {
				finished <- err
				return
			}
			if request["command"] != "getInterfaceOrientation" {
				finished <- os.ErrInvalid
				return
			}
			if err := codec.Write(map[string]interface{}{"interfaceOrientation": value}); err != nil {
				finished <- err
				return
			}
		}
		finished <- nil
	}()
	for _, want := range []int32{3, 4, 0, 0} {
		got, err := channel.read()
		if want == 0 && err == nil || want != 0 && (err != nil || got != want) {
			t.Fatalf("got=%d err=%v want=%d", got, err, want)
		}
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	// Closing the session interrupts a query even if SpringBoard never replies.
	reading := make(chan error, 1)
	go func() { _, err := channel.read(); reading <- err }()
	cancel()
	select {
	case err := <-reading:
		if err == nil {
			t.Fatal("cancelled query succeeded")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("orientation query did not stop")
	}
}

func TestUSBInterfaceOrientation(t *testing.T) {
	wanted := os.Getenv("ANYKVM_TEST_COREDEVICE_ORIENTATION")
	if wanted == "" {
		t.Skip("set ANYKVM_TEST_COREDEVICE_ORIENTATION=1 or the active phone UDID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	devices, err := listUSB(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var matches []ios.DeviceEntry
	for _, device := range devices.DeviceList {
		if device.Properties.ConnectionType == "USB" && (wanted == "1" || device.Properties.SerialNumber == wanted) {
			matches = append(matches, device)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected one selected USB phone, got %d", len(matches))
	}
	channel, err := openOrientationChannel(ctx, &endpoint{udid: matches[0].Properties.SerialNumber})
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	orientation, err := channel.read()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("phone interface orientation=%d", orientation)
}
