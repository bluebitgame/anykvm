package viewer

import (
	"anykvm/internal/bridge"
	"anykvm/internal/capture"
	"testing"
)

func TestLatestFrameOwnsSourceData(t *testing.T) {
	d := NewDevice()
	defer d.Close()
	data := []byte{1, 2, 3, 4}
	d.PushFrame(bridge.Frame{Data: data, Width: 1, Height: 1})
	data[0] = 9
	d.PushFrame(bridge.Frame{Data: data, Width: 2, Height: 1})
	data[0] = 7
	f, err := d.Capture()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	if f.Width != 2 || f.Data[0] != 9 {
		t.Fatalf("queued frame = %+v", f)
	}
}

func TestCloseUnblocksCapture(t *testing.T) {
	d := NewDevice()
	done := make(chan error, 1)
	go func() { _, err := d.Capture(); done <- err }()
	d.Close()
	d.Close()
	if err := <-done; err != capture.ErrStreamStopped {
		t.Fatalf("Capture error = %v", err)
	}
	d.PushFrame(bridge.Frame{Data: []byte{1}})
}

func TestCloseReasonDistinguishesRemoteTakeover(t *testing.T) {
	local := NewDevice()
	local.Close()
	if local.TakenOver() {
		t.Fatal("local close was treated as a remote takeover")
	}
	remote := NewDevice()
	remote.CloseClients()
	if !remote.TakenOver() {
		t.Fatal("remote takeover was not recorded")
	}
}
